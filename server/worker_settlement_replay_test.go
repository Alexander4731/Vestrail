package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"github.com/Autumn-27/norma/llm"
)

func TestWorkerSettlesAfterPendingProcessReceipt(t *testing.T) {
	for _, state := range []string{"done", "stopped"} {
		t.Run(state, func(t *testing.T) {
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
			id, err := task.Store.AddIntent(map[string]any{"summary": "verify fixture"}, 5, nil, "planner")
			if err != nil {
				t.Fatal(err)
			}
			if err := task.Store.SetNodeState(id, "running"); err != nil {
				t.Fatal(err)
			}
			intent, err := task.Store.GetNode(id)
			if err != nil {
				t.Fatal(err)
			}
			audit := db.InterceptAudit{IntentID: id, RunID: "fixture-run", ToolUseID: "fixture-call", EffectiveAction: "allow", ExecutionStatus: "unknown", InputDigest: "fixture"}
			approval, err := m.pg.CreateDecidedIntercept(0, 0, task.ID, "fixture", "Bash", json.RawMessage(`{}`), "allowed", "fixture", &audit)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.pg.PrepareToolExecution(t.Context(), approval, audit.RunID, audit.ToolUseID, m.dir, false); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(m.dir, "transcripts", "execution-receipts")
			outputDir := filepath.Join(root, fmt.Sprint(approval))
			if err := os.MkdirAll(outputDir, 0700); err != nil {
				t.Fatal(err)
			}
			outputPath := filepath.Join(outputDir, "process-output.txt")
			if err := os.WriteFile(outputPath, []byte("real fixture failure"), 0600); err != nil {
				t.Fatal(err)
			}
			receipt, _ := json.Marshal(map[string]any{"id": approval, "run_id": audit.RunID, "call_id": audit.ToolUseID, "event": map[string]any{"kind": "process_exited", "status": "failed"}, "output": "real fixture failure"})
			pending := filepath.Join(root, fmt.Sprintf("%d-fixture.pending.json", approval))
			if err := os.WriteFile(pending, receipt, 0600); err != nil {
				t.Fatal(err)
			}
			oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
			agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = nil, nil, nil
			defer func() { agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt }()
			e := NewEngine(m)
			calls := 0
			provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if state == "stopped" {
					if err := e.KillWork(id); err != nil {
						t.Fatal(err)
					}
				} else if calls == 1 {
					input, _ := json.Marshal(map[string]any{"summary": "fixture verified", "intent_id": id, "evidence": "local synthetic fixture", "confidence": "observed"})
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "record", Name: "record_fact", Input: input}}}, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Fixture finished.")}}, "end_turn", llm.Usage{}, nil
			}}
			worker := agent.NewWorker(provider, "fixture", m.dir, nil, 32000, 5)
			worker.SetNonStreaming(func() bool { return true })
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			e.runIntent(ctx, task, "fixture", worker, intent, "", "")
			got, err := task.Store.GetNode(id)
			if err != nil || got.State != state || e.IsPaused(task.ID) {
				t.Fatalf("intent=%+v err=%v taskPaused=%v", got, err, e.IsPaused(task.ID))
			}
			if e.work[id] != nil {
				t.Fatal("settled worker retained its execution handle")
			}
			execution, err := m.pg.GetExecution(t.Context(), task.ID, approval)
			if err != nil || execution.ExecutionState != "failed" || execution.Output != "real fixture failure" {
				t.Fatalf("execution=%+v err=%v", execution, err)
			}
			if raw, err := os.ReadFile(outputPath); err != nil || string(raw) != "real fixture failure" {
				t.Fatalf("process output=%q err=%v", raw, err)
			}
			if _, err := os.Stat(pending); !os.IsNotExist(err) {
				t.Fatalf("committed pending receipt retained: %v", err)
			}
		})
	}
}
