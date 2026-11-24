package server

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"testing"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type fixedToolProvider struct {
	toolName string
	input    string
	calls    int
}

func (p *fixedToolProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if p.calls == 1 {
		return llm.Message{
			Role: llm.RoleAssistant,
			Content: []llm.ContentBlock{{
				Type:  llm.BlockToolUse,
				ID:    "call_1",
				Name:  p.toolName,
				Input: []byte(p.input),
			}},
		}, "tool_use", llm.Usage{InputTokens: 10, OutputTokens: 5}, nil
	}
	return llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock("done")},
	}, "end_turn", llm.Usage{InputTokens: 10, OutputTokens: 5}, nil
}

func (p *fixedToolProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, u, _ := p.Complete(ctx, req)
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: u.InputTokens}}, nil) {
			return
		}
		for _, b := range msg.Content {
			if b.Type == llm.BlockToolUse {
				yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil)
				yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil)
			} else {
				yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil)
			}
		}
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: u.OutputTokens}}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

// F18: Empty write results delivered through the real norma/harness PostToolUse
// JSON-string wrapper must NOT reset stagnantTurns.
func TestAuditFix_EmptyWriteThroughRealSDKHarness(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	for _, payload := range []string{`{"ids":[]}`, `{"inserted":0}`, `{"results":[]}`} {
		t.Run(payload, func(t *testing.T) {
			guard := newWorkerFailureGuard(nil)
			guard.stagnantTurns = 19
			hooks := steerHooks{failures: guard}
			fakeWrite := actool.Build(actool.Spec{
				Name: "record_fact",
				Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
					return actool.Text(payload), nil
				},
			})
			prov := &fixedToolProvider{toolName: "record_fact", input: `{"summary":"noop"}`}
			for _, err := range harness.Query(t.Context(), harness.QueryInput{
				Provider:     prov,
				NonStreaming: true,
				Messages:     []llm.Message{llm.UserText("write")},
				Tools:        actool.NewRegistry(fakeWrite),
				Hooks:        hooks,
			}, harness.QueryDeps{}) {
				if err != nil {
					t.Fatal(err)
				}
			}
			if guard.stagnantTurns != 19 {
				t.Fatalf("empty write %s through SDK harness cleared stagnantTurns to %d", payload, guard.stagnantTurns)
			}
		})
	}
}

// F19: 50 distinct valid negative batches (both plain text and vestrail.batch.v1)
// must never trigger stagnation steering/blocking, while true duplicates and invalid
// batches still increment stagnation.
func TestAuditFix_NewNegativeCoverageNotStagnant(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	for i := 0; i < 50; i++ {
		start := 10001 + i*500
		end := start + 499
		in := []byte(fmt.Sprintf(`{"command":"scan --start %d --end %d"}`, start, end))
		if blocked, reason := g.before("Bash", in); blocked {
			t.Fatalf("batch %d (%d-%d) wrongly blocked: %s", i, start, end, reason)
		}
		out := fmt.Sprintf("valid=true; range=%d-%d; attempted=500; positive=0", start, end)
		wrapped, _ := json.Marshal(out)
		g.after("Bash", in, false, wrapped)
	}
	if g.stagnantTurns != 0 {
		t.Fatalf("distinct valid negative batches accrued stagnantTurns=%d", g.stagnantTurns)
	}

	// Structured vestrail.batch.v1 invalid batch must NOT reset stagnation
	g2 := newWorkerFailureGuard(nil)
	g2.probeTurns = 30
	g2.stagnantTurns = 10
	invalidBatch, _ := json.Marshal(`{"schema":"vestrail.batch.v1","range_start":20001,"range_end":20500,"valid":false,"precondition_ok":false,"attempted":500,"unique_covered":0,"positive":0}`)
	g2.after("RunScript", []byte(`{"path":"scan.py"}`), false, invalidBatch)
	if g2.stagnantTurns != 11 {
		t.Fatalf("invalid batch should increment stagnation to 11, got %d", g2.stagnantTurns)
	}
	validBatch, _ := json.Marshal(`{"schema":"vestrail.batch.v1","range_start":20001,"range_end":20500,"valid":true,"precondition_ok":true,"attempted":500,"unique_covered":500,"positive":0}`)
	g2.after("RunScript", []byte(`{"path":"scan.py"}`), false, validBatch)
	if g2.stagnantTurns != 0 {
		t.Fatalf("valid unseen batch should reset stagnation to 0, got %d", g2.stagnantTurns)
	}
}

// Valid partial delivery remains research gain independently of execution state.
func TestAuditFix_ExhaustedNetGainKeepsConcurrency(t *testing.T) {
	e := NewEngine(nil)
	for range 4 {
		receipt := kernel.RunReceipt{
			SettledState: "exhausted",
			ResearchGain: &kernel.ResearchGain{Evidence: 1},
		}
		if !receipt.HasNetGain() {
			t.Fatal("valid partial delivery lost its gain")
		}
	}
	for id := int64(1); id <= 4; id++ {
		if !e.resources.tryAcquire(intentResourceRequest{intentID: id, taskID: "task-f20", locks: map[string]bool{}}, true) {
			t.Fatal("independent work must remain admissible")
		}
	}
}

// F01: Planner local budget exhaustion must NOT be classified as a retryable model error.
func TestAuditFix_PlannerLocalBudgetNotRetryable(t *testing.T) {
	budgetErr := &agent.BudgetTimeoutError{Code: kernel.TermBudgetRun, Cause: agent.AbortRunHardTimeout}
	if retryablePlannerModelError(budgetErr) {
		t.Fatal("BudgetTimeoutError was misclassified as retryable planner model error")
	}
	if retryablePlannerModelError(agent.AbortRunHardTimeout) {
		t.Fatal("AbortRunHardTimeout was misclassified as retryable planner model error")
	}
	if !retryablePlannerModelError(context.DeadlineExceeded) {
		t.Fatal("genuine upstream DeadlineExceeded should remain retryable")
	}
}

// F17: Per-intent max_tool_calls blocks further probe tools while leaving write-back tools open.
func TestAuditFix_PerIntentToolBudgetEnforced(t *testing.T) {
	g := newWorkerFailureGuardWithBudget(nil, 6)
	for i := 0; i < 6; i++ {
		if blocked, reason := g.before("Bash", []byte(fmt.Sprintf(`{"command":"cmd %d"}`, i))); blocked {
			t.Fatalf("call %d unexpectedly blocked: %s", i+1, reason)
		}
		g.recordAdmittedProbe("Bash")
		g.after("Bash", []byte(fmt.Sprintf(`{"command":"cmd %d"}`, i)), false, []byte(`"ok"`))
	}
	if blocked, _ := g.before("Bash", []byte(`{"command":"cmd 7"}`)); !blocked {
		t.Fatal("7th probe call should have been blocked by max_tool_calls=6")
	}
	if blocked, reason := g.before("record_fact", []byte(`{"summary":"final"}`)); blocked {
		t.Fatalf("write-back tool must remain open after max_tool_calls reached: %s", reason)
	}
}

// F21: Shared channel pool divides concurrency across concurrent workers and restores on release.
func TestAuditFix_SharedChannelPoolBudget(t *testing.T) {
	t.Setenv("VESTRAIL_SHARED_CHANNEL_CAP", "8")
	pool := newChannelPoolTracker()
	key := pool.poolKeyFor("9", "http://127.0.0.1:8080", &db.Node{ID: 2676})
	_, active1, budget1, rel1 := pool.acquire(key, 2676)
	if active1 != 1 || budget1 != 8 {
		t.Fatalf("first worker got active=%d budget=%d", active1, budget1)
	}
	_, active2, budget2, rel2 := pool.acquire(key, 2677)
	if active2 != 2 || budget2 != 4 {
		t.Fatalf("second worker got active=%d budget=%d, want 2/4", active2, budget2)
	}
	rel1()
	_, active3, budget3, rel3 := pool.acquire(key, 2678)
	if active3 != 2 || budget3 != 4 {
		t.Fatalf("third worker got active=%d budget=%d, want 2/4", active3, budget3)
	}
	rel2()
	rel3()
}
