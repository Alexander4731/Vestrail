package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	actool "github.com/Autumn-27/norma/tool"
)

func TestRunBudgetObserverUsesConfiguredHardDeadline(t *testing.T) {
	var observed time.Time
	ctx := WithRunBudgetObserver(t.Context(), func(deadline time.Time) { observed = deadline })
	buffer := 180
	plan := kernel.ComputeBudgetPlanWithPolicy(30, 0, time.Now(), kernel.BudgetPolicy{HardBufferSeconds: &buffer})
	ctx, cancel := runBudgetDeadline(ctx, plan)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || observed.IsZero() || !observed.Equal(deadline) || time.Until(deadline) < 209*time.Second {
		t.Fatal("observer missed configured deadline")
	}
}

func TestVerifiedInputBatchesSupportRemainingDelivery(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration(t.Name(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	store := d.Exploration(exp)
	toolset := NewToolSet(store, "worker")
	toolset.SetExecutionContract(t.TempDir(), kernel.IntentContract{Scope: "ports 51-100", Deliverable: kernel.BatchSchemaV1})
	id, err := store.AddNode(db.KindFact, map[string]any{"summary": "fixture observations"}, 5, "confirmed", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := kernel.EvidenceBatch{Schema: kernel.BatchSchemaV1, Target: "example.test", Method: "tcp", RangeStart: 1, RangeEnd: 100, Attempted: 100, UniqueCovered: 100, Valid: true, PreconditionOK: true}
	body, _ := json.Marshal(batch)
	path := filepath.Join(t.TempDir(), "batch.jsonl")
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	_, verified, err := VerifyArtifact(t.Context(), "", kernel.ArtifactRef{Path: path, Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRunEvidence(t.Context(), func() []kernel.EvidenceBatch { return verified }, nil)
	raw, _ := json.Marshal(map[string]any{"delivery_status": "complete", "summary": "remaining range covered", "evidence_ids": []int64{id}})
	result, err := toolset.finishWork().Call(ctx, raw, nil)
	if err != nil || result.IsError || toolset.Writes().DeliveryStatus != kernel.DeliveryComplete {
		t.Fatalf("delivery=%s err=%v", result.Flatten(), err)
	}
	if gain := toolset.Writes().ResearchGain; gain.Coverage != 0 || gain.Evidence != 0 {
		t.Fatal("input evidence credited as new")
	}
	if result, _ := toolset.finishWork().Call(context.Background(), raw, nil); !result.IsError {
		t.Fatal("bounded complete accepted without actual coverage")
	}
}

func TestContinuationUsesOnlyRemainingScope(t *testing.T) {
	r := &kernel.RunReceipt{Checkpoint: &kernel.Checkpoint{RemainingScope: "ports 51-100"}}
	if !continuationCoversRemaining("ports 51-100", r) || !continuationCoversRemaining("ports 70-100", r) || continuationCoversRemaining("ports 1-100", r) {
		t.Fatal("continuation range validation failed")
	}
	r.Checkpoint.RemainingScope = "example.test ports 51-100 tcp"
	if continuationCoversRemaining("other.test ports 70-100 tcp", r) || continuationCoversRemaining("example.test ports 70-100 udp", r) {
		t.Fatal("continuation changed the target or method")
	}
}

func TestRunScriptBatchPreviewReducesInputWithoutLosingRawOutput(t *testing.T) {
	dir := t.TempDir()
	manager, err := actool.NewManager("batch-preview-fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Cleanup()
	script := "import json\nfor port in range(1,501):\n print(json.dumps(dict(schema='vestrail.batch.v1',scope='fixture ports 1-500',target='example.test',method='tcp',range_start=port,range_end=port,valid=True,precondition_ok=True,attempted=1,unique_covered=1,positive=0)))\n"
	raw, _ := json.Marshal(map[string]any{"path": "fixture.py", "content": script})
	outputDir := filepath.Join(dir, "output")
	result, err := NewRunScriptTool().Call(t.Context(), raw, &actool.ToolContext{WorkingDir: dir, Tasks: manager, OutputDir: outputDir, MaxOutputChars: 30000})
	if err != nil || result.IsError {
		t.Fatalf("script=%s err=%v", result.Flatten(), err)
	}
	files, err := filepath.Glob(filepath.Join(outputDir, "output-*.txt"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archives=%v err=%v", files, err)
	}
	original, err := os.ReadFile(files[0])
	if err != nil || strings.Count(string(original), kernel.BatchSchemaV1) != 500 {
		t.Fatal("raw batch records not preserved")
	}
	before := actool.Capture(&actool.ToolContext{OutputDir: filepath.Join(dir, "before"), MaxOutputChars: 30000}, string(original))
	after := result.Flatten()
	if len(after) >= len(before)/10 || !strings.Contains(after, "<persisted-output>") {
		t.Fatal("batch preview did not materially reduce model input")
	}
	tracker := kernel.NewCoverageTracker("fixture")
	tracker.Seed(kernel.ExtractEvidenceBatches(after))
	if !tracker.CoversRange(1, 500) {
		t.Fatal("actual tool preview lost complete coverage")
	}
	t.Logf("actual local RunScript: ordinary preview=%d bytes, batch preview=%d bytes; 500 original records retained", len(before), len(after))
}

func TestRepeatedAssetRegistrationCountsOnceAndCanComplete(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration(t.Name(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	toolset := NewToolSet(d.Exploration(exp), "worker")
	toolset.SetAssetStore(d.Assets(), d.Companies())
	toolset.SetExecutionContract(t.TempDir(), kernel.IntentContract{Scope: "fixture domain inventory", Deliverable: "asset registration"})
	body := json.RawMessage(`{"assets":[{"type":"root_domain","domain":"asset-progress-fixture.invalid"}]}`)
	var id int64
	for i := 0; i < 2; i++ {
		result, err := toolset.insertAssets().Call(t.Context(), body, nil)
		if err != nil || result.IsError {
			t.Fatalf("insert=%s err=%v", result.Flatten(), err)
		}
		var out struct {
			First []int64 `json:"first_written_this_run"`
		}
		if err := json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if len(out.First) != 1 {
				t.Fatal("first write missing")
			}
			id = out.First[0]
		} else if len(out.First) != 0 {
			t.Fatal("duplicate upsert credited")
		}
	}
	defer d.Assets().DeleteByIDs([]int64{id})
	if toolset.Writes().Assets != 1 {
		t.Fatal("same asset counted more than once")
	}
	result, err := toolset.finishWork().Call(t.Context(), json.RawMessage(`{"delivery_status":"complete","summary":"asset inventory registered"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("asset delivery=%s err=%v", result.Flatten(), err)
	}
}

func TestVerifyArtifactReadsBatchesBeyondPreviewAndRejectsInvalidTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	for port := int64(1); port <= 65535; port++ {
		batch := kernel.EvidenceBatch{Schema: kernel.BatchSchemaV1, Scope: "example.test ports 1-65535", Target: "example.test", Method: "tcp", Valid: true, PreconditionOK: true, RangeStart: port, RangeEnd: port, Attempted: 1, UniqueCovered: 1}
		if err := encoder.Encode(batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	verified, batches, err := VerifyArtifact(t.Context(), "", kernel.ArtifactRef{Path: path, Valid: true, Provenance: "target_response"})
	if err != nil || verified.Size <= 8*1024*1024 || len(batches) != 1 || !CompleteScopeCovered("ports 1-65535", batches) {
		t.Fatalf("size=%d batches=%d err=%v", verified.Size, len(batches), err)
	}
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":"vestrail.batch.v1","valid":false,"precondition_ok":false,"attempted":1,"unique_covered":0}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := VerifyArtifact(t.Context(), "", kernel.ArtifactRef{Path: path, Valid: true}); err == nil {
		t.Fatal("invalid batch beyond the preview accepted")
	}
}

func TestSourceArtifactSchemaLiteralIsNotBatchEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scan.py")
	if err := os.WriteFile(path, []byte("print({'schema':'vestrail.batch.v1'})\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ref, batches, err := VerifyArtifact(t.Context(), "", kernel.ArtifactRef{Path: path, Valid: true, Provenance: "source_code"})
	if err != nil || !ref.Valid || len(batches) != 0 {
		t.Fatalf("source=%+v batches=%d err=%v", ref, len(batches), err)
	}
}

func TestWorkerRequestsMissingFinalReceiptAsContinuation(t *testing.T) {
	hooks := &workerDeliveryHooks{tools: NewToolSet(nil, "worker")}
	stop, blocking, _ := hooks.Stop(t.Context(), nil)
	if stop || len(blocking) != 1 {
		t.Fatal("missing receipt terminated instead of requesting a continuation")
	}
}

func TestFinishWorkPreservesPriorCheckpointAndReturnsGain(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	exp, err := d.CreateExploration(t.Name(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	ts := NewToolSet(d.Exploration(exp), "worker")
	ts.updateWrites(func(w *WriteCounts) {
		w.Checkpoint = &kernel.Checkpoint{RemainingScope: "remaining fixture"}
		w.ResearchGain.Evidence = 1
	})
	result, err := ts.finishWork().Call(t.Context(), json.RawMessage(`{"delivery_status":"partial_checkpoint","summary":"handoff"}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("result=%s err=%v", result.Flatten(), err)
	}
	var out struct {
		Gain kernel.ResearchGain `json:"research_gain"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
		t.Fatal(err)
	}
	if ts.Writes().Checkpoint == nil || ts.Writes().Checkpoint.RemainingScope != "remaining fixture" || out.Gain.Evidence != 1 {
		t.Fatalf("writes=%+v gain=%+v", ts.Writes(), out.Gain)
	}
}

func TestWorkerPromptsMentionFinishWork(t *testing.T) {
	for _, prompt := range []string{workerDefaultTmpl, workerFocusContract, settleWrapUpPrompt, workerTaskTimeoutDefault} {
		if !strings.Contains(prompt, "finish_work") {
			t.Fatal("worker prompt lacks finish_work")
		}
	}
}

func TestScopeBatchAndArtifactBoundaries(t *testing.T) {
	batch := kernel.EvidenceBatch{Schema: kernel.BatchSchemaV1, Valid: true, PreconditionOK: true, Attempted: 10, UniqueCovered: 10, RangeStart: 100, RangeEnd: 109, Method: "tcp", Target: "fixture"}
	if err := ValidateScopeBatches("ports 1-10 and ports 100-110", []kernel.EvidenceBatch{batch}); err != nil {
		t.Fatal(err)
	}
	batch.RangeEnd = 111
	if err := ValidateScopeBatches("ports 1-10 and ports 100-110", []kernel.EvidenceBatch{batch}); err == nil {
		t.Fatal("out-of-scope range accepted")
	}
	batch.RangeStart, batch.RangeEnd = 0, 0
	if err := ValidateScopeBatches("ports 1-10", []kernel.EvidenceBatch{batch}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, body := range []string{"log mentions vestrail.batch.v1 as documentation", `[{"schema":"vestrail.batch.v1","valid":true,"precondition_ok":true,"attempted":1,"unique_covered":1,"range_start":1,"range_end":1}]`} {
		path := filepath.Join(dir, "artifact.log")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, batches, err := VerifyArtifact(t.Context(), dir, kernel.ArtifactRef{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(body, "[") && len(batches) != 1 {
			t.Fatal("JSON array batch lost")
		}
	}
	tracker := kernel.NewCoverageTracker("ports 1-10")
	batch.RangeStart, batch.RangeEnd = 1, 10
	tracker.Add(batch)
	batch.Method, batch.RangeEnd, batch.Attempted, batch.UniqueCovered = "http_probe", 1, 1, 1
	tracker.Add(batch)
	if !tracker.CoversRange(1, 10) {
		t.Fatal("secondary method suppressed full coverage")
	}
}
