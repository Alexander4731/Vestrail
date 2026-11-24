package server

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Alexander4731/Vestrail/intercept"
)

type phaseActivity struct {
	mu     sync.Mutex
	counts map[string]int
}

// ApprovalPausedWorker describes a single worker suspended on a high-risk
// approval while the rest of its task and sibling workers continue running.
type ApprovalPausedWorker struct {
	TaskID    string    `json:"task_id"`
	IntentID  int64     `json:"intent_id"`
	Worker    string    `json:"worker"`
	PendingID int64     `json:"pending_id"`
	ToolName  string    `json:"tool_name"`
	Reason    string    `json:"reason"`
	Since     time.Time `json:"since"`
}

func (e *Engine) markWorkerApprovalPaused(notice intercept.PendingNotice) {
	if notice.IntentID <= 0 {
		return
	}
	e.approvalMu.Lock()
	if e.approvalPaused == nil {
		e.approvalPaused = map[int64]ApprovalPausedWorker{}
	}
	e.approvalPaused[notice.IntentID] = ApprovalPausedWorker{
		TaskID:    notice.TaskID,
		IntentID:  notice.IntentID,
		Worker:    notice.AgentName,
		PendingID: notice.PendingID,
		ToolName:  notice.ToolName,
		Reason:    notice.Reason,
		Since:     notice.CreatedAt,
	}
	e.approvalMu.Unlock()
	if notice.TaskID != "" {
		e.touch(notice.TaskID)
	}
}

func (e *Engine) clearWorkerApprovalPaused(intentID int64, taskID string) {
	if intentID <= 0 {
		return
	}
	e.approvalMu.Lock()
	delete(e.approvalPaused, intentID)
	e.approvalMu.Unlock()
	if taskID != "" {
		e.touch(taskID)
	}
}

// ApprovalPausedWorkers returns all workers of taskID currently paused for
// high-risk approval (empty slice when none).
func (e *Engine) ApprovalPausedWorkers(taskID string) []ApprovalPausedWorker {
	e.approvalMu.Lock()
	defer e.approvalMu.Unlock()
	out := make([]ApprovalPausedWorker, 0)
	for _, w := range e.approvalPaused {
		if taskID == "" || w.TaskID == taskID {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IntentID < out[j].IntentID })
	return out
}

func (e *Engine) phaseContext(ctx context.Context, taskID string) context.Context {
	return intercept.WithPhaseObserver(ctx, func(phase string, delta int) {
		v, _ := e.phases.LoadOrStore(taskID, &phaseActivity{counts: map[string]int{}})
		a := v.(*phaseActivity)
		a.mu.Lock()
		a.counts[phase] = max(0, a.counts[phase]+delta)
		a.mu.Unlock()
		e.touch(taskID)
	})
}

func (e *Engine) executionPhase(taskID string) string {
	if e.isSettling(taskID) {
		return "settling"
	}
	var approvalCount int
	if v, ok := e.phases.Load(taskID); ok {
		a := v.(*phaseActivity)
		a.mu.Lock()
		// Active sibling workers ("tool", "settling", "backoff") keep the task in
		// active execution rather than making the whole task appear stalled on one
		// worker's approval. Only report task-level "approval" when no other active
		// tool/LLM work is running.
		for _, phase := range []string{"tool", "settling", "backoff"} {
			if a.counts[phase] > 0 {
				a.mu.Unlock()
				return phase
			}
		}
		approvalCount = a.counts["approval"]
		a.mu.Unlock()
	}
	if e.ActiveLLMCalls(taskID) > 0 {
		return "model"
	}
	if approvalCount > 0 {
		return "approval"
	}
	return "idle"
}
