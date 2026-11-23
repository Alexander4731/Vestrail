package kernel

import (
	"testing"
	"time"
)

func TestComputeBudgetPlan(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// 0 = truly unlimited (no 5-minute fallback)
	if plan := ComputeBudgetPlan(0, 0, now); plan.Total != 0 || plan.Explore != 0 || plan.SettleReserve <= 0 || plan.ClampedByTask {
		t.Fatalf("expected unlimited plan, got %+v", plan)
	}

	// 20-minute run -> 90s settle reserve, 18m30s explore, 15s hard buffer
	plan := ComputeBudgetPlan(1200, 0, now)
	if plan.Total != 1200*time.Second || plan.SettleReserve != 90*time.Second || plan.Explore != 1110*time.Second || plan.HardBuffer != 15*time.Second || plan.ClampedByTask {
		t.Fatalf("unexpected 20m plan: %+v", plan)
	}

	// Clamped by task deadline (remaining 300s < own 1200s) -> 60s settle reserve, 240s explore
	clamped := ComputeBudgetPlan(1200, now.Add(300*time.Second).Unix(), now)
	if !clamped.ClampedByTask || clamped.Total != 300*time.Second || clamped.SettleReserve != 60*time.Second || clamped.Explore != 240*time.Second {
		t.Fatalf("unexpected clamped plan: %+v", clamped)
	}

	// Own unlimited but task deadline present -> clamped
	taskOnly := ComputeBudgetPlan(0, now.Add(100*time.Second).Unix(), now)
	if !taskOnly.ClampedByTask || taskOnly.Total != 100*time.Second || taskOnly.SettleReserve != 20*time.Second || taskOnly.Explore != 80*time.Second {
		t.Fatalf("unexpected task-only plan: %+v", taskOnly)
	}
}

func TestRunReceiptNetGainAndHandback(t *testing.T) {
	if (RunReceipt{SettledState: "done"}).HasNetGain() {
		t.Fatal("empty done receipt must not report net gain")
	}
	if !(RunReceipt{SettledState: "exhausted", FactsWritten: 1}).HasNetGain() {
		t.Fatal("exhausted receipt with written fact must report net gain")
	}
	if !(RunReceipt{
		SettledState: "exhausted",
		ValidBatches: []EvidenceBatch{{Schema: BatchSchemaV1, Valid: true, PreconditionOK: true, UniqueCovered: 500}},
	}).HasNetGain() {
		t.Fatal("exhausted receipt with valid negative batch must report net gain")
	}
	if (RunReceipt{
		SettledState: "exhausted",
		ValidBatches: []EvidenceBatch{{Schema: BatchSchemaV1, Valid: false, PreconditionOK: true, UniqueCovered: 500}},
	}).HasNetGain() {
		t.Fatal("invalid batch must not report net gain")
	}
	if !LooksLikeIncompleteHandback("I have to stop here and hand this back rather than continue.") {
		t.Fatal("expected handback text to be detected")
	}
	if LooksLikeIncompleteHandback("Confirmed port 8080 returns 404 across all 50 paths.") {
		t.Fatal("normal conclusion should not look like handback")
	}
}

func TestEvidenceBatchAndProjectionAndEpistemics(t *testing.T) {
	good := EvidenceBatch{
		Schema: BatchSchemaV1, RangeStart: 10001, RangeEnd: 10500,
		Valid: true, PreconditionOK: true, Attempted: 500, UniqueCovered: 500, Positive: 0,
	}
	if err := ValidateBatch(good); err != nil {
		t.Fatalf("valid batch rejected: %v", err)
	}
	badRange := good
	badRange.UniqueCovered = 501
	if err := ValidateBatch(badRange); err == nil {
		t.Fatal("expected oversized unique_covered to fail")
	}
	badInvalid := good
	badInvalid.Valid = false
	if err := ValidateBatch(badInvalid); err == nil {
		t.Fatal("expected invalid batch with unique_covered > 0 to fail")
	}

	extracted := ExtractEvidenceBatches("log line\n{\"schema\":\"vestrail.batch.v1\",\"range_start\":10001,\"range_end\":10500,\"valid\":true,\"precondition_ok\":true,\"attempted\":500,\"unique_covered\":500,\"positive\":0}\ndone")
	if len(extracted) != 1 || extracted[0].CanonicalKey() != "range:10001-10500" {
		t.Fatalf("unexpected extracted batches: %+v", extracted)
	}

	proj := ProjectFactCompact(2400, "confirmed", map[string]any{
		"summary":      "候选机理可能成立",
		"confidence":   "inferred",
		"provenance":   "source_code",
		"reentry_when": "获取管理员会话后复测",
	}, 160)
	if proj["confidence"] != "inferred" || proj["provenance"] != "source_code" || proj["reentry_when"] != "获取管理员会话后复测" {
		t.Fatalf("projection dropped metadata: %+v", proj)
	}
	missingConf := ProjectFactCompact(2401, "confirmed", map[string]any{"summary": "old fact"}, 160)
	if missingConf["confidence"] != "unspecified" {
		t.Fatalf("expected unspecified confidence, got %+v", missingConf)
	}

	conf, note := NormalizeFactEpistemics("控制端口正常，其他失败不是代理问题", "", "observed", nil)
	if conf != "inferred" || note == "" {
		t.Fatalf("expected local control observation exclusion downgrade, got (%q, %q)", conf, note)
	}
}

func TestIntentDedupAndResourceLocks(t *testing.T) {
	a := DedupCandidate{Summary: "测试 /api/admin 鉴权绕过", Brief: "检查 X-Forwarded-For 头。", AssetIDs: []int64{10}}
	b := DedupCandidate{Summary: "测试 /api/admin 鉴权绕过", Brief: "检查 X-Forwarded-For 头！", AssetIDs: []int64{10}}
	overlap, threshold := CheckIntentOverlap(a, b)
	if !overlap || IntentSimilarity(a.Summary, b.Summary) < threshold {
		t.Fatalf("punctuation change bypassed dedup: overlap=%v threshold=%v", overlap, threshold)
	}

	// Distinct scope is allowed
	s1 := DedupCandidate{Summary: "扫描端口段", Scope: "10001-10500", AssetIDs: []int64{10}}
	s2 := DedupCandidate{Summary: "扫描端口段", Scope: "10501-11000", AssetIDs: []int64{10}}
	if ov, _ := CheckIntentOverlap(s1, s2); ov {
		t.Fatal("distinct explicit scopes must not collide")
	}

	// Fresh parent evidence check
	if HasFreshParentEvidence([]int64{100, 150}, 200, map[int64]bool{100: true}) {
		t.Fatal("older parents must not count as fresh evidence")
	}
	if !HasFreshParentEvidence([]int64{100, 205}, 200, map[int64]bool{100: true}) {
		t.Fatal("parent newer than completed intent must count as fresh evidence")
	}

	// Resource RW-lock & after_intents (F22)
	cleanup := IntentScheduleInfo{
		ID: 2950, State: "open",
		ResourceLocks: []ResourceLock{{Resource: "channel:tmp-1", Mode: "exclusive"}},
	}
	consumerRunning := IntentScheduleInfo{
		ID: 2957, State: "running",
		ResourceLocks: []ResourceLock{{Resource: "channel:tmp-1", Mode: "shared"}},
	}
	consumerOpen := IntentScheduleInfo{
		ID: 2963, State: "open",
		ResourceLocks: []ResourceLock{{Resource: "channel:tmp-1", Mode: "shared"}},
	}
	if ok, _ := CanClaimIntent(cleanup, []IntentScheduleInfo{cleanup, consumerRunning}, nil); ok {
		t.Fatal("exclusive cleanup must wait for running shared consumer")
	}
	if ok, _ := CanClaimIntent(cleanup, []IntentScheduleInfo{cleanup, consumerOpen}, nil); !ok {
		t.Fatal("queued work must not hold a resource")
	}
	cleanup.AfterIntents = []int64{consumerOpen.ID}
	if ok, _ := CanClaimIntent(cleanup, []IntentScheduleInfo{cleanup, consumerOpen}, nil); ok {
		t.Fatal("cleanup must wait for its explicit consumer dependency")
	}
	cleanup.AfterIntents = nil
	if ok, _ := CanClaimIntent(cleanup, []IntentScheduleInfo{cleanup}, nil); !ok {
		t.Fatal("exclusive cleanup should proceed once no shared consumers remain")
	}
	if ok, _ := CanClaimIntent(consumerOpen, []IntentScheduleInfo{consumerRunning, consumerOpen}, nil); !ok {
		t.Fatal("shared consumers must run concurrently")
	}
	if got := WorkerConcurrencyBudget(8, 2); got != 4 {
		t.Fatalf("expected concurrency split 4, got %d", got)
	}
	if got := WorkerConcurrencyBudget(2, 5); got != 1 {
		t.Fatalf("expected concurrency floor 1, got %d", got)
	}
}
