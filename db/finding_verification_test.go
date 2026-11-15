package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestVerificationReceiptOwnershipAndCommitFence(t *testing.T) {
	d, task := reportTestTask(t)
	s := d.Exploration(task.ExplorationID)
	intent, err := s.AddNode(KindIntent, map[string]any{"goal": "local fixture"}, 0, "done", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.RecordFinding(t.Context(), RecordFindingInput{TaskID: task.ID, IntentID: intent, VulnClass: "fixture", Summary: "claim", Severity: "low", Worker: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteFinding(f.FindingID) })
	audit := &InterceptAudit{RunID: "verification-run", ToolUseID: "call", IntentID: intent, InputDigest: "fixture-digest", Correlation: "exact", EffectiveAction: "allow", ExecutionStatus: "awaiting_result"}
	receiptID, err := d.CreateDecidedIntercept(0, 0, fmt.Sprint(task.ID), "test", "Read", []byte(`{"file_path":"fixture"}`), "allowed", "fixture", audit)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.PrepareToolExecution(t.Context(), receiptID, "verification-run", "call", t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	if err = d.StoreExecutionEvent(t.Context(), receiptID, "returned", "succeeded", "fixture output", false, json.RawMessage(`{"event":1}`)); err != nil {
		t.Fatal(err)
	}
	var receipt *VerificationReceipt
	if err = d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		var e error
		receipt, e = VerificationReceiptTx(t.Context(), tx, f.FindingID, receiptID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !receipt.Complete || receipt.RunID != "verification-run" || receipt.OutputSHA256 == "" {
		t.Fatalf("receipt missing provenance %+v", receipt)
	}
	other, err := d.AddFinding(task.ID, 0, "fixture", "unbound", "low", "claim", "evidence", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteFinding(other)
	if err = d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error { _, e := VerificationReceiptTx(t.Context(), tx, other, receiptID); return e }); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("receipt reused for unrelated finding", err)
	}
	a, _, err := d.BeginVerification(t.Context(), f.FindingID, 0, "receipt-check", "execution_receipt", "human:test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.StoreExecutionEvent(t.Context(), receiptID, "returned", "succeeded", "late output", false, json.RawMessage(`{"event":2}`)); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishVerification(t.Context(), a.ID, VerificationDecision{Outcome: "supported", Assurance: "evidence_bound", Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	a, err = d.GetVerification(t.Context(), f.FindingID, a.ID)
	if err != nil || a.Outcome != "inconclusive" || a.Assurance != "none" {
		t.Fatalf("stale receipt committed %+v %v", a, err)
	}
	if err = d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
		var e error
		receipt, e = VerificationReceiptTx(t.Context(), tx, f.FindingID, receiptID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	a, _, err = d.BeginVerification(t.Context(), f.FindingID, 0, "completed-receipt", "execution_receipt", "human:test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishVerification(t.Context(), a.ID, VerificationDecision{Outcome: "supported", Assurance: "evidence_bound", Receipt: receipt}); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.GetFinding(f.FindingID)
	if err != nil || fresh.Verification().Stale {
		t.Fatalf("current receipt marked stale: %+v %v", fresh, err)
	}
	before, err := d.FindingStats()
	if err != nil {
		t.Fatal(err)
	}
	if err = d.StoreExecutionEvent(t.Context(), receiptID, "returned", "succeeded", "output after verification", false, json.RawMessage(`{"event":3}`)); err != nil {
		t.Fatal(err)
	}
	stale, err := d.GetFinding(f.FindingID)
	if err != nil || !stale.Verification().Stale || stale.EvidenceVersion != fresh.EvidenceVersion {
		t.Fatalf("completed check missed late receipt: %+v %v", stale, err)
	}
	meta, err := d.FindingMetaByNodeID(task.ID)
	if err != nil || !meta[f.NodeID].Verification.Stale {
		t.Fatal("node metadata missed stale receipt", err)
	}
	report, err := d.TaskReport(t.Context(), task.ID)
	if err != nil || len(report.Findings) != 1 || !report.Findings[0].FindingVerification.Stale {
		t.Fatalf("report missed stale receipt: %+v %v", report, err)
	}
	after, err := d.FindingStats()
	if err != nil || after.VerificationStale != before.VerificationStale+1 {
		t.Fatalf("stale receipt statistics differ: %+v %v", after, err)
	}
	a, err = d.GetVerification(t.Context(), f.FindingID, a.ID)
	if err != nil || a.Outcome != "supported" || a.Assurance != "evidence_bound" {
		t.Fatalf("freshness read rewrote historical decision: %+v %v", a, err)
	}
}

func TestVerificationDeduplicationAuthorityAndStaleness(t *testing.T) {
	d, fid := retestDB(t)
	f, err := d.GetFinding(fid)
	if err != nil || f.Verification().State != "candidate" {
		t.Fatalf("new finding: %+v %v", f, err)
	}
	var wg sync.WaitGroup
	ids := make(chan int64, 8)
	for range 8 {
		wg.Go(func() {
			a, _, err := d.BeginVerification(t.Context(), fid, 0, "retry-key", "evidence_integrity", "human:test", json.RawMessage(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- a.ID
		})
	}
	wg.Wait()
	close(ids)
	var id int64
	for got := range ids {
		if id != 0 && got != id {
			t.Fatal("duplicate attempts")
		}
		id = got
	}
	if id == 0 {
		t.Fatal("no attempt")
	}
	if _, _, err = d.BeginVerification(t.Context(), fid, 0, "retry-key", "evidence_integrity", "human:other", json.RawMessage(`{}`)); !errors.Is(err, ErrVerificationConflict) {
		t.Fatal("key reused by another actor", err)
	}
	if _, _, err = d.BeginVerification(t.Context(), fid, 0, "retry-key", "evidence_integrity", "human:test", json.RawMessage(`{"different":1}`)); !errors.Is(err, ErrVerificationConflict) {
		t.Fatal("key changed content", err)
	}
	if _, _, err = d.BeginVerification(t.Context(), fid, 0, "another", "evidence_integrity", "human:test", json.RawMessage(`{}`)); !errors.Is(err, ErrVerificationConflict) {
		t.Fatal("parallel run", err)
	}
	if err = d.FinishVerification(t.Context(), id, VerificationDecision{Outcome: "supported", Assurance: "semantic_verified"}); err == nil {
		t.Fatal("integrity granted semantic verification")
	}
	if err = d.FinishVerification(t.Context(), id, VerificationDecision{Outcome: "inconclusive", Assurance: "evidence_bound"}); err == nil {
		t.Fatal("inconclusive decision retained positive assurance")
	}
	if err = d.FinishVerification(t.Context(), id, VerificationDecision{Outcome: "supported", Assurance: "evidence_bound", Reason: "complete files"}); err != nil {
		t.Fatal(err)
	}
	f, _ = d.GetFinding(fid)
	if f.Status != FindingPending || f.Verification().State != "candidate" || f.Verification().Assurance != "evidence_bound" {
		t.Fatalf("evidence promoted vulnerability: %+v", f)
	}
	if _, err = d.Exec(`UPDATE findings SET evidence='new claim' WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	f, _ = d.GetFinding(fid)
	if !f.Verification().Stale || f.EvidenceVersion != 1 {
		t.Fatal("claim edits did not invalidate proof")
	}
	if err = d.FinishVerification(t.Context(), id, VerificationDecision{Outcome: "not_supported", Assurance: "none"}); !errors.Is(err, ErrVerificationConflict) {
		t.Fatal("overwrote historical result", err)
	}
}

func TestVerificationVersionFenceAndRecovery(t *testing.T) {
	d, fid := retestDB(t)
	a, _, err := d.BeginVerification(t.Context(), fid, 0, "first", "controlled_marker", "host:test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Exec(`UPDATE findings SET evidence_version=evidence_version+1 WHERE id=$1`, fid); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishVerification(t.Context(), a.ID, VerificationDecision{Outcome: "supported", Assurance: "semantic_verified"}); err != nil {
		t.Fatal(err)
	}
	f, _ := d.GetFinding(fid)
	if f.VerificationState != "inconclusive" || f.VerificationAssurance != "none" || !f.Verification().Stale {
		t.Fatalf("stale verification won: %+v", f)
	}
	a, _, err = d.BeginVerification(t.Context(), fid, 1, "second", "evidence_integrity", "host:test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := d.AcquireRuntimeLock(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = d.InterruptVerifications(t.Context()); err != nil {
		t.Fatal(err)
	}
	a, err = d.GetVerification(t.Context(), fid, a.ID)
	if err != nil || a.State != "interrupted" || a.Assurance != "none" {
		t.Fatalf("recovery: %+v %v", a, err)
	}
}

func TestVerificationLegacyArchiveDefaults(t *testing.T) {
	raw, err := normalizeArchivedFindingVersions(json.RawMessage(`[{"status":"confirmed"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err = json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0]["verification_state"] != "legacy_unverified" || rows[0]["status"] != "confirmed" || rows[0]["status_revision"] != float64(0) {
		t.Fatal(string(raw))
	}
}
