package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	actool "github.com/Autumn-27/norma/tool"
)

// F02/F03: Unified budget resolution for Worker, Planner, MainAgent (0 = truly unlimited).
func TestAuditFix_RoleRunSecondsContract(t *testing.T) {
	if plan := resolveBudgetPlan(0, TaskClock{}); plan.Total != 0 || plan.Explore != 0 || plan.SettleReserve <= 0 {
		t.Fatalf("run_seconds=0 must stay unlimited, got %+v", plan)
	}
	plan := resolveBudgetPlan(1200*time.Second, TaskClock{})
	if plan.Total != 1200*time.Second || plan.SettleReserve != 90*time.Second || plan.Explore != 1110*time.Second {
		t.Fatalf("unexpected 1200s budget plan: %+v", plan)
	}
	// MainAgent SetRunTimeout wires cleanly
	m := NewMainAgent(nil, "m", t.TempDir(), nil, 200000, 0)
	m.SetRunTimeout(900 * time.Second)
	if m.runTimeout != 900*time.Second {
		t.Fatalf("MainAgent.SetRunTimeout did not store duration: %v", m.runTimeout)
	}
}

// F06: Worker default toolset restores native Glob and Grep (only MultiEdit excluded).
func TestAuditFix_WorkerHasNativeGlobAndGrep(t *testing.T) {
	tools := defaultToolsExcept("MultiEdit")
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	if !names["Glob"] || !names["Grep"] {
		t.Fatalf("Worker toolset missing native Glob/Grep: %+v", names)
	}
	if names["MultiEdit"] {
		t.Fatal("MultiEdit should remain excluded")
	}
	if actool.NewBash() == nil {
		t.Fatal("NewBash returned nil")
	}
}

// F11: Compact fact projection preserves confidence/provenance/reentry_when.
func TestAuditFix_FactConfidencePreserved(t *testing.T) {
	n := &db.Node{
		ID:      2400,
		Kind:    db.KindFact,
		State:   "confirmed",
		Payload: json.RawMessage(`{"summary":"候选机理可能成立，仍需验证","confidence":"inferred","provenance":"source_code","reentry_when":"取得会话后复测"}`),
	}
	cf := compactFact(n)
	if cf["confidence"] != "inferred" || cf["provenance"] != "source_code" || cf["reentry_when"] != "取得会话后复测" {
		t.Fatalf("compactFact dropped fields: %+v", cf)
	}
}

// F15: Brief punctuation edits cannot bypass intent deduplication.
func TestAuditFix_BriefPunctuationDoesNotBypassDedup(t *testing.T) {
	existing := &db.Node{
		ID:      101,
		Kind:    db.KindIntent,
		State:   "open",
		Payload: json.RawMessage(`{"summary":"验证 /api/admin 权限绕过","brief":"测试 X-Original-URL 头部。","asset_ids":[7]}`),
	}
	in := intentItem{
		Summary:  "验证 /api/admin 权限绕过",
		Brief:    "测试 X-Original-URL 头部！",
		AssetIDs: []json.RawMessage{json.RawMessage(`7`)},
	}
	overlap, threshold := intentsOverlap(in, existing)
	if !overlap || intentSimilarity(in.Summary, "验证 /api/admin 权限绕过") < threshold {
		t.Fatalf("punctuation tweak bypassed dedup: overlap=%v threshold=%v", overlap, threshold)
	}
}

// F13/F14: Planner default template aligns parent_ids contract and drops empty-queue hard compulsion.
func TestAuditFix_PlannerPromptContractsAligned(t *testing.T) {
	if strings.Contains(plannerDefaultTmpl, "派生自某上游意图/发现也传其 id") {
		t.Fatal("plannerDefaultTmpl still tells model to pass intent id into parent_ids (F13)")
	}
	if !strings.Contains(plannerDefaultTmpl, "continue_intent_id") {
		t.Fatal("plannerDefaultTmpl missing continue_intent_id guidance (F13)")
	}
	if strings.Contains(plannerDefaultTmpl, "硬底线（最高优先）") {
		t.Fatal("plannerDefaultTmpl still contains rigid empty-queue compulsion (F14)")
	}
	if !strings.Contains(plannerDefaultTmpl, "list_facts(queries=") {
		t.Fatal("plannerDefaultTmpl missing batch multi-query list_facts guidance (F09)")
	}
}

// F07: Upstream RunReceipt renders structured checkpoint handoff.
func TestAuditFix_UpstreamReceiptHandoffRendering(t *testing.T) {
	block := renderUpstreamReceiptHandoff(3048, &kernel.RunReceipt{
		Checkpoint: &kernel.Checkpoint{
			CompletedScope: "39 names tested",
			RemainingScope: "candidate C",
			NextAction:     "send minimal probe for candidate C",
			ValidArtifacts: []kernel.ArtifactRef{{Path: "candidates.json", Provenance: "source_code", Valid: true}},
		},
	})
	for _, want := range []string{"#3048", "39 names tested", "candidate C", "candidates.json", "source_code"} {
		if !strings.Contains(block, want) {
			t.Fatalf("handoff block missing %q: %s", want, block)
		}
	}
}

// F29: Role-specific compaction config supports per-role A/B overrides without changing defaults.
func TestAuditFix_RoleCompactionOverrides(t *testing.T) {
	t.Setenv("VESTRAIL_WORKER_SOFT_WINDOW", "96000")
	t.Setenv("VESTRAIL_WORKER_MICRO_GROWTH", "24000")
	t.Setenv("VESTRAIL_WORKER_TOOL_OUTPUT_CHARS", "8000")
	cfg := compactionConfigForRole("worker", 200000)
	if cfg.SoftWindow != 96000 || cfg.MicroBatchGrowth != 24000 {
		t.Fatalf("worker compaction overrides ignored: %+v", cfg)
	}
	if got := toolOutputCharsForRole("worker"); got != 8000 {
		t.Fatalf("worker tool output cap override ignored: %d", got)
	}
	plannerCfg := compactionConfigForRole("planner", 200000)
	if plannerCfg.SoftWindow == 96000 {
		t.Fatalf("worker override leaked into planner config: %+v", plannerCfg)
	}
}
