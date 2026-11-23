package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/llm"
)

func TestRepeatedCommandCommentsEnterSettlement(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	var requested kernel.TerminationCode
	g.setSettlementRequester(func(code kernel.TerminationCode) { requested = code })
	for i := 0; i < 80; i++ {
		input := []byte(fmt.Sprintf(`{"command":"echo unchanged # comment-%d"}`, i))
		if deny, _ := g.before("Bash", input); deny && requested != "" {
			break
		}
		g.after("Bash", input, false, []byte(`"unchanged result"`))
	}
	if requested != kernel.TermNoProgress {
		t.Fatalf("requested=%q stagnant=%d", requested, g.stagnantTurns)
	}
	if deny, _ := g.before("finish_work", nil); deny {
		t.Fatal("settlement write-back blocked")
	}
}

func TestCompletedDeliveryDuringSettlementKeepsDoneState(t *testing.T) {
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = nil, nil, nil
	t.Cleanup(func() { agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt })
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	artifact := filepath.Join(t.TempDir(), "proof.txt")
	if err := os.WriteFile(artifact, []byte("local validated delivery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := task.Store.AddIntent(map[string]any{"summary": "local artifact delivery"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(m)
	node := e.claimNext(task, "worker")
	if node == nil || node.ID != id {
		t.Fatal("claim failed")
	}
	calls := 0
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		name, input := "Read", map[string]any{"file_path": artifact}
		if calls > 1 {
			name = "finish_work"
			input = map[string]any{"delivery_status": "complete", "summary": "local artifact delivered", "checkpoint": map[string]any{"valid_artifacts": []kernel.ArtifactRef{{Path: artifact, Valid: true}}}}
		}
		raw, _ := json.Marshal(input)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: fmt.Sprint(calls), Name: name, Input: raw}}}, "tool_use", llm.Usage{}, nil
	}}
	worker := agent.NewWorker(provider, "fixture", m.dir, nil, 32000, 1)
	worker.SetNonStreaming(func() bool { return true })
	e.runIntent(t.Context(), task, "worker", worker, node, "", "")
	node, err = task.Store.GetNode(id)
	receipt, receiptErr := task.Store.LatestIntentReceipt(t.Context(), id)
	if err != nil || receiptErr != nil || node.State != "done" || receipt == nil || receipt.DeliveryStatus != kernel.DeliveryComplete || receipt.TerminationCode != kernel.TermBudgetRun || calls != 2 {
		t.Fatalf("node=%+v receipt=%+v calls=%d errors=%v/%v", node, receipt, calls, err, receiptErr)
	}
}

func TestEmptyWritesRequestBoundedSettlement(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	var requested kernel.TerminationCode
	g.setSettlementRequester(func(code kernel.TerminationCode) { requested = code })
	for range 30 {
		g.after("record_fact", nil, false, []byte(`{"id":123,"research_gain":{"evidence":0,"coverage":0,"artifacts":0}}`))
	}
	if requested != kernel.TermNoProgress {
		t.Fatalf("requested=%q", requested)
	}
}

func TestFailedToolOutputCannotClaimCoverage(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	data := []byte(`{"schema":"vestrail.batch.v1","scope":"target","range_start":100,"range_end":199,"attempted":100,"unique_covered":100,"valid":true,"precondition_ok":true}`)
	g.after("Bash", nil, true, data)
	if g.coverageDelta() != 0 {
		t.Fatal("failed tool was credited")
	}
}

func TestRepeatedAssetUpsertDoesNotResetProgress(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	g.stagnantTurns = 15
	g.after("insert_assets", nil, false, []byte(`{"results":[{"id":123}],"first_written_this_run":[]}`))
	if g.stagnantTurns < 15 {
		t.Fatal("repeated upsert reset stagnation")
	}
	g.after("insert_assets", nil, false, []byte(`{"results":[{"id":456}],"first_written_this_run":[456]}`))
	if g.stagnantTurns != 0 {
		t.Fatal("first asset write did not advance work")
	}
}

func TestCoverageBaselinePreservesTargetAndMethod(t *testing.T) {
	if !sameCoverageScope("example.test ports 51-100 tcp", "example.test ports 1-100 tcp") {
		t.Fatal("remaining slice lost reusable coverage")
	}
	if sameCoverageScope("other.test ports 51-100 tcp", "example.test ports 1-100 tcp") || sameCoverageScope("example.test ports 51-100 udp", "example.test ports 1-100 tcp") {
		t.Fatal("different target or method reused prior coverage")
	}
}
