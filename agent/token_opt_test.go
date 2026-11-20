package agent

import (
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestWorkerStaticPrefixIdenticalAcrossIntents(t *testing.T) {
	baseStatic := workerStaticSystem("http://127.0.0.1:8788", "/ca.pem", "/data")
	def := DeferredInfo{
		Deferred:    []string{"node_detail", "traffic_blob"},
		GlobalNames: []string{"node_detail", "traffic_blob"},
	}

	sys1, b1 := deferredSystemWithDynamic(baseStatic, "意图 #101\n目录: /data/tasks/1/i101", def)
	sys2, b2 := deferredSystemWithDynamic(baseStatic, "意图 #202\n目录: /data/tasks/1/i202", def)

	if b1 <= 0 || b1 != b2 {
		t.Fatalf("expected matching positive cache boundaries: b1=%d b2=%d", b1, b2)
	}
	if len(sys1) != b1+1 || len(sys2) != b2+1 {
		t.Fatalf("expected 1 dynamic tail segment after boundary: len1=%d len2=%d b=%d", len(sys1), len(sys2), b1)
	}
	for i := 0; i < b1; i++ {
		if sys1[i] != sys2[i] {
			t.Fatalf("static prefix segment %d diverged across intents:\n1: %q\n2: %q", i, sys1[i], sys2[i])
		}
	}
	if sys1[b1] == sys2[b2] {
		t.Fatal("dynamic intent tail should differ across intents")
	}
	if strings.Contains(sys1[0], "i101") || strings.Contains(sys2[0], "i202") {
		t.Fatal("per-intent directory must not leak into static cached prefix")
	}
}

func TestDeferBuiltinTools(t *testing.T) {
	ts := NewToolSet(nil, "worker")
	tools := append(ts.WorkerTools(), defaultToolsExcept("MultiEdit", "Glob", "Grep")...)

	// Simulate an existing skill-gated MCP tool that starts locked.
	existingUnlock := actool.NewUnlockSet("mcp__global__ping")
	def := DeferredInfo{
		Deferred:    []string{"mcp__global__ping", "mcp__secret__exploit"},
		GlobalNames: []string{"mcp__global__ping"},
		Unlock:      existingUnlock,
	}

	out := deferBuiltinTools(tools, def, "node_detail", "sleep", "nonexistent_tool", "node_detail")

	if !out.Unlock.Has("node_detail") || !out.Unlock.Has("sleep") {
		t.Fatal("deferred built-in tools present in toolset must be unlocked")
	}
	if out.Unlock.Has("mcp__secret__exploit") {
		t.Fatal("skill-gated MCP tool must remain locked")
	}
	if out.Unlock.Has("nonexistent_tool") {
		t.Fatal("tool absent from toolset must not be deferred or unlocked")
	}
	count := 0
	for _, n := range out.Deferred {
		if n == "node_detail" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("node_detail duplicated in Deferred: %v", out.Deferred)
	}
}

func TestCompactionConfigTunedDefaults(t *testing.T) {
	cfg := compactionConfig(200000)
	if cfg.ContextWindow != 200000 {
		t.Fatalf("ContextWindow=%d want 200000", cfg.ContextWindow)
	}
	if cfg.SoftWindow != defaultSoftWindow {
		t.Fatalf("SoftWindow=%d want %d", cfg.SoftWindow, defaultSoftWindow)
	}
	if cfg.MicroBatchGrowth != defaultMicroBatchGrowth {
		t.Fatalf("MicroBatchGrowth=%d want %d", cfg.MicroBatchGrowth, defaultMicroBatchGrowth)
	}
	if !cfg.ConciseSummary {
		t.Fatal("expected ConciseSummary=true")
	}
	seen := map[string]bool{}
	for _, n := range cfg.ExtraCompactable {
		seen[n] = true
	}
	for _, want := range []string{"traffic_get", "list_assets", "node_detail", "ExecuteExtraTool"} {
		if !seen[want] {
			t.Errorf("ExtraCompactable missing %q", want)
		}
	}
	// Smaller context windows should not have SoftWindow exceed ContextWindow.
	small := compactionConfig(32000)
	if small.SoftWindow != 32000 {
		t.Fatalf("small SoftWindow=%d want 32000", small.SoftWindow)
	}
}

func TestTruncRuneSummary(t *testing.T) {
	short := "简短结论"
	if got := truncRuneSummary(short, 10); got != short {
		t.Fatalf("short string modified: %v", got)
	}
	long := strings.Repeat("测", 200)
	got, ok := truncRuneSummary(long, 140).(string)
	if !ok || len([]rune(got)) != 140 || !strings.HasSuffix(got, "…") {
		t.Fatalf("unexpected truncation: len=%d val=%q", len([]rune(got)), got)
	}
	if got := truncRuneSummary(123, 10); got != 123 {
		t.Fatalf("non-string modified: %v", got)
	}
}

func TestPlannerDefaultToolSlimming(t *testing.T) {
	kept := defaultToolsExcept("Write", "Edit", "MultiEdit", "sleep")
	for _, tool := range kept {
		switch tool.Name() {
		case "Write", "Edit", "MultiEdit", "sleep":
			t.Fatalf("planner should not include %s in default tools", tool.Name())
		}
	}
	foundBash := false
	for _, tool := range kept {
		if tool.Name() == "Bash" {
			foundBash = true
		}
	}
	if !foundBash {
		t.Fatal("planner should keep Bash for opening-round light recon (deferred once facts exist)")
	}
	_ = actool.NewUnlockSet()
}
