package intercept

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestExecutionEventSpoolsOutsideHistoricalOutputDirectories(t *testing.T) {
	sqlDB, err := sql.Open("pgx", "postgres://unused/unused")
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, trace := WithTrace(t.Context(), "fixture", nil)
	root := t.TempDir()
	trace.UseTranscript(transcript.NewStore(root))
	trace.Start("call", "Bash", json.RawMessage(`{}`))
	trace.bindExecution(&db.DB{DB: sqlDB}, 42, db.InterceptAudit{RunID: "run", ToolUseID: "call"}, "Bash")
	err = trace.observeExecution(ctx, tool.ExecutionEvent{ToolUseID: "call", ToolName: "Bash", Kind: "returned", Status: "failed", Output: "actual failure"})
	if err == nil {
		t.Fatal("closed database must leave a receipt for recovery")
	}
	paths, err := filepath.Glob(filepath.Join(root, "execution-receipts", "42-*.pending.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("flat pending receipts=%v err=%v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt spooledReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.ID != 42 || receipt.RunID != "run" || receipt.CallID != "call" || receipt.Output != "actual failure" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestRuntimeReceiptReplayDoesNotTraverseHistoricalOutputs(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "execution-receipts", "42", "legacy.pending.json")
	if err := writeReceiptFile(legacy, []byte(`{"id":42,"run_id":"run","call_id":"call"}`)); err != nil {
		t.Fatal(err)
	}
	// A legacy envelope would require a database lookup if runtime replay read it.
	if err := ReplayExecutionReceipts(t.Context(), nil, root, "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("legacy evidence was not retained for startup recovery", err)
	}
}

func TestReceiptReplayHonorsCancellationWithoutPendingFiles(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ReplayExecutionReceipts(ctx, nil, t.TempDir(), "8"); !errors.Is(err, context.Canceled) {
		t.Fatalf("replay error=%v, want context.Canceled", err)
	}
}

func TestReceiptReplayTaskScopeAndLegacyStartup(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("test database not configured")
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	root := t.TempDir()
	create := func(name string) (int64, int64) {
		t.Helper()
		task, err := d.CreateTask(name, "fixture", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
		audit := db.InterceptAudit{RunID: "run", ToolUseID: name, InputDigest: "fixture", EffectiveAction: "allow", ExecutionStatus: "awaiting_result"}
		id, err := d.CreateDecidedIntercept(0, 0, fmt.Sprint(task.ID), "worker", "Bash", json.RawMessage(`{}`), "allowed", "fixture", &audit)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.PrepareToolExecution(t.Context(), id, "run", name, root, false); err != nil {
			t.Fatal(err)
		}
		return task.ID, id
	}
	firstTask, firstID := create(t.Name() + "-first")
	secondTask, secondID := create(t.Name() + "-second")
	write := func(id int64, call, kind, status, path string) {
		t.Helper()
		raw, err := json.Marshal(spooledReceipt{ID: id, RunID: "run", CallID: call, Event: tool.ExecutionEvent{Kind: kind, Status: status}, Output: status})
		if err != nil {
			t.Fatal(err)
		}
		if err := writeReceiptFile(path, raw); err != nil {
			t.Fatal(err)
		}
	}
	firstFlat := filepath.Join(root, "execution-receipts", fmt.Sprintf("%d-start.pending.json", firstID))
	secondFlat := filepath.Join(root, "execution-receipts", fmt.Sprintf("%d-exit.pending.json", secondID))
	legacy := filepath.Join(root, "execution-receipts", fmt.Sprint(firstID), "exit.pending.json")
	write(firstID, t.Name()+"-first", "process_started", "running", firstFlat)
	write(firstID, t.Name()+"-first", "process_exited", "failed", legacy)
	write(secondID, t.Name()+"-second", "process_exited", "succeeded", secondFlat)
	if err := ReplayExecutionReceipts(t.Context(), d, root, fmt.Sprint(firstTask)); err != nil {
		t.Fatal(err)
	}
	first, err := d.GetExecution(t.Context(), fmt.Sprint(firstTask), firstID)
	if err != nil || first.ExecutionState != "running" {
		t.Fatalf("first execution=%+v err=%v", first, err)
	}
	second, err := d.GetExecution(t.Context(), fmt.Sprint(secondTask), secondID)
	if err != nil || second.ExecutionState != "prepared" {
		t.Fatalf("other task execution=%+v err=%v", second, err)
	}
	if err := ReplayExecutionReceipts(t.Context(), d, root); err != nil {
		t.Fatal(err)
	}
	first, err = d.GetExecution(t.Context(), fmt.Sprint(firstTask), firstID)
	if err != nil || first.ExecutionState != "failed" || first.Output != "failed" {
		t.Fatalf("legacy execution=%+v err=%v", first, err)
	}
	second, err = d.GetExecution(t.Context(), fmt.Sprint(secondTask), secondID)
	if err != nil || second.ExecutionState != "succeeded" {
		t.Fatalf("second execution=%+v err=%v", second, err)
	}
	for _, path := range []string{firstFlat, secondFlat, legacy} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("committed receipt %s retained: %v", path, err)
		}
	}
}
