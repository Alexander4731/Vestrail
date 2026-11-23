package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/Alexander4731/Vestrail/intercept"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	actool "github.com/Autumn-27/norma/tool"
)

// ErrLocalRunBudgetExceeded signals that a run stopped because its own local
// wall-clock budget (or task deadline clamp) expired — never a transient model outage.
var ErrLocalRunBudgetExceeded = errors.New("单次运行预算已耗尽")

// BudgetTimeoutError wraps a local hard-timeout abort cause with its kernel TerminationCode.
type BudgetTimeoutError struct {
	Code  kernel.TerminationCode
	Cause error
}

func (e *BudgetTimeoutError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", ErrLocalRunBudgetExceeded.Error(), e.Cause)
	}
	return ErrLocalRunBudgetExceeded.Error()
}

func (e *BudgetTimeoutError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrLocalRunBudgetExceeded, e.Cause}
	}
	return []error{ErrLocalRunBudgetExceeded}
}

type extraBashEnvKey struct{}

// WithExtraBashEnv appends extra KEY=VALUE pairs (e.g. shared channel concurrency budget)
// to a worker's subprocess environment without mutating shared Worker state (F21).
func WithExtraBashEnv(ctx context.Context, env []string) context.Context {
	if len(env) == 0 {
		return ctx
	}
	return context.WithValue(ctx, extraBashEnvKey{}, append([]string(nil), env...))
}

func extraBashEnvFrom(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(extraBashEnvKey{}).([]string)
	return v
}

// resolveBudgetPlan computes the unified Explore/Settle/Hard budget plan for any
// role (Worker, Planner, MainAgent). ownTimeout <= 0 means unlimited (F02/F03).
func resolveBudgetPlan(ownTimeout time.Duration, tc TaskClock) kernel.BudgetPlan {
	ownSec := int(ownTimeout / time.Second)
	if ownTimeout > 0 && ownSec <= 0 {
		ownSec = 1
	}
	return kernel.ComputeBudgetPlan(ownSec, tc.DeadlineUnix, time.Now())
}

// runBudgetDeadline attaches the exploration approval deadline and final hard backstop
// (Total + HardBuffer) using the unified two-phase BudgetPlan (F01/F03).
func runBudgetDeadline(ctx context.Context, plan kernel.BudgetPlan) (context.Context, context.CancelFunc) {
	if plan.Total <= 0 {
		return ctx, func() {}
	}
	now := time.Now()
	ctx = intercept.WithApprovalDeadline(ctx, now.Add(plan.Explore))
	return context.WithDeadlineCause(ctx, now.Add(plan.HardDuration()), AbortRunHardTimeout)
}

// runDeadline is retained for existing internal tests/callers.
func runDeadline(ctx context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
	if duration <= 0 {
		return ctx, func() {}
	}
	plan := kernel.ComputeBudgetPlan(max(1, int(duration/time.Second)), 0, time.Now())
	return runBudgetDeadline(ctx, plan)
}

func resumeSession(s *agentcore.Session, id string) error {
	if err := s.Resume(id); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("恢复会话 %s 失败: %w", id, err)
	}
	return nil
}

var finalPlannerAllowed = map[string]bool{
	"graph_overview": true, "list_goals": true, "list_facts": true,
	"list_findings": true, "get_worker_output": true, "get_worker_trace": true,
	"search_all_worker_traces": true, "prove_goal": true, "goal_met": true,
}

type finalPlannerHooks struct{ harness.HookRunner }

func (h finalPlannerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if !finalPlannerAllowed[name] {
		return true, "终局判定仅允许读取证据和确认目标", nil
	}
	return h.HookRunner.PreToolUse(ctx, name, input)
}

func finalPlannerTools(tools []actool.CoreTool) []actool.CoreTool {
	out := make([]actool.CoreTool, 0, len(tools))
	for _, tool := range tools {
		if finalPlannerAllowed[tool.Name()] {
			out = append(out, tool)
		}
	}
	return out
}
