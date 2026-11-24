package intercept

import (
	"context"
	"sync"
	"time"
)

type phaseKey struct{}

// PendingNotice describes a newly created high-risk approval request so both
// per-worker pause tracking and notification channels can react immediately.
type PendingNotice struct {
	PendingID int64     `json:"pending_id"`
	TaskID    string    `json:"task_id"`
	IntentID  int64     `json:"intent_id"`
	AgentName string    `json:"agent_name"`
	ToolName  string    `json:"tool_name"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

type approvalHooksKey struct{}

type approvalPauseHooks struct {
	onEnter func(PendingNotice)
	onExit  func(pendingID int64)
}

// WithApprovalPauseHooks attaches per-worker callbacks invoked around HandleAsk
// waiting so ONLY the triggering worker pauses and yields shared channel slots
// while the rest of the task and sibling workers continue running.
func WithApprovalPauseHooks(ctx context.Context, onEnter func(PendingNotice), onExit func(pendingID int64)) context.Context {
	return context.WithValue(ctx, approvalHooksKey{}, approvalPauseHooks{onEnter: onEnter, onExit: onExit})
}

func approvalPauseHooksFromCtx(ctx context.Context) (approvalPauseHooks, bool) {
	h, ok := ctx.Value(approvalHooksKey{}).(approvalPauseHooks)
	return h, ok
}

func WithPhaseObserver(ctx context.Context, observe func(string, int)) context.Context {
	return context.WithValue(ctx, phaseKey{}, observe)
}

func BeginPhase(ctx context.Context, phase string) func() {
	observe, _ := ctx.Value(phaseKey{}).(func(string, int))
	if observe == nil {
		return func() {}
	}
	observe(phase, 1)
	var once sync.Once
	return func() { once.Do(func() { observe(phase, -1) }) }
}
