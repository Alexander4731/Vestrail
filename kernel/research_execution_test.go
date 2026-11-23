package kernel

import (
	"testing"
	"time"
)

func TestConfiguredPhaseBudgetAndExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	zero := 0
	plan := ComputeBudgetPlanWithPolicy(300, 0, now, BudgetPolicy{SettleSeconds: 60, HardBufferSeconds: &zero})
	if plan.Explore != 240*time.Second || plan.SettleReserve != time.Minute || plan.HardDuration() != 300*time.Second {
		t.Fatalf("configured budget=%+v", plan)
	}
	expired := ComputeBudgetPlan(300, now.Add(-time.Second).Unix(), now)
	if !expired.TaskExpired || expired.Total != 0 {
		t.Fatalf("expired=%+v", expired)
	}
	short := ComputeBudgetPlan(1, 0, now)
	if short.Explore+short.SettleReserve != short.Total || short.Explore <= 0 {
		t.Fatalf("short=%+v", short)
	}
}

func TestCoverageCountsOnlyNewObjectSets(t *testing.T) {
	tracker := NewCoverageTracker("target ports")
	batch := EvidenceBatch{Schema: BatchSchemaV1, Scope: "target ports", Target: "example.test", Method: "tcp", RangeStart: 100, RangeEnd: 199, Attempted: 100, UniqueCovered: 100, Valid: true, PreconditionOK: true}
	if n := tracker.Add(batch); n != 100 {
		t.Fatalf("first=%d", n)
	}
	batch.BatchKey = "different label"
	if n := tracker.Add(batch); n != 0 {
		t.Fatalf("repeat=%d", n)
	}
	batch.RangeStart, batch.RangeEnd = 150, 249
	if n := tracker.Add(batch); n != 50 {
		t.Fatalf("overlap=%d", n)
	}
	batch.Target = "other.test"
	if n := tracker.Add(batch); n != 100 {
		t.Fatalf("target=%d", n)
	}
	batch.Method = "udp"
	if n := tracker.Add(batch); n != 100 {
		t.Fatalf("method=%d", n)
	}
	batch.UniqueCovered = 20
	if n := tracker.Add(batch); n != 0 {
		t.Fatalf("unknown partial=%d", n)
	}
	batch.Valid, batch.UniqueCovered = false, 0
	if n := tracker.Add(batch); n != 0 {
		t.Fatalf("invalid=%d", n)
	}
}

func TestCheckpointMetadataDoesNotManufactureResearchGain(t *testing.T) {
	r := RunReceipt{FactsWritten: 1, ResearchGain: &ResearchGain{}, Checkpoint: &Checkpoint{RemainingScope: "same remaining range", ValidArtifacts: []ArtifactRef{{Path: "claimed", Valid: true}}}}
	if r.HasNetGain() {
		t.Fatal("metadata counted as verified gain")
	}
	r.ResearchGain.Coverage = 20
	if !r.HasNetGain() {
		t.Fatal("valid negative coverage lost")
	}
}
