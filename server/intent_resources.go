package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
)

func sameCoverageScope(current, previous string) bool {
	if strings.Join(strings.Fields(current), " ") == strings.Join(strings.Fields(previous), " ") {
		return true
	}
	currentContext, currentBounded := kernel.PortScopeContext(current)
	previousContext, previousBounded := kernel.PortScopeContext(previous)
	return currentBounded && previousBounded && currentContext == previousContext
}

type intentResourceRequest struct {
	taskID   string
	intentID int64
	priority int
	state    string
	locks    map[string]bool // true = exclusive
}

type intentResourceTracker struct {
	mu      sync.Mutex
	held    map[int64]intentResourceRequest
	waiting map[int64]intentResourceRequest
	changed chan struct{}
}

func newIntentResourceTracker() *intentResourceTracker {
	return &intentResourceTracker{held: map[int64]intentResourceRequest{}, waiting: map[int64]intentResourceRequest{}, changed: make(chan struct{})}
}

func resourceRequest(taskID string, in *db.Node, expected string) intentResourceRequest {
	r := intentResourceRequest{taskID: taskID, intentID: in.ID, priority: in.Priority, state: expected, locks: map[string]bool{}}
	for _, lock := range intentScheduleInfoFromNode(in).ResourceLocks {
		key := strings.ToLower(strings.TrimSpace(lock.Resource))
		if key == "" {
			continue
		}
		// Opaque channels do not expose their internal TCP concurrency to the
		// recording proxy. One consumer owns such a channel for the whole run.
		exclusive := strings.EqualFold(lock.Mode, "exclusive") || strings.HasPrefix(key, "channel:")
		r.locks[key] = r.locks[key] || exclusive
	}
	return r
}

func requestsConflict(a, b intentResourceRequest) bool {
	for key, exclusive := range a.locks {
		if other, ok := b.locks[key]; ok && (exclusive || other) {
			return true
		}
	}
	return false
}

func (p *intentResourceTracker) tryAcquire(r intentResourceRequest) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, held := p.held[r.intentID]; held {
		return false
	}
	p.waiting[r.intentID] = r
	for _, holder := range p.held {
		if requestsConflict(r, holder) {
			return false
		}
	}
	for id, waiting := range p.waiting {
		if id == r.intentID || !requestsConflict(r, waiting) {
			continue
		}
		if waiting.priority > r.priority || (waiting.priority == r.priority && id < r.intentID) {
			return false
		}
	}
	delete(p.waiting, r.intentID)
	p.held[r.intentID] = r
	return true
}

func (p *intentResourceTracker) release(id int64) {
	p.mu.Lock()
	_, held := p.held[id]
	_, waiting := p.waiting[id]
	delete(p.held, id)
	delete(p.waiting, id)
	if held || waiting {
		close(p.changed)
		p.changed = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *intentResourceTracker) forgetWaiting(id int64) {
	p.mu.Lock()
	if _, ok := p.waiting[id]; ok {
		delete(p.waiting, id)
		close(p.changed)
		p.changed = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *intentResourceTracker) pending(taskID string) map[int64]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[int64]string{}
	for id, r := range p.waiting {
		if r.taskID == taskID {
			out[id] = r.state
		}
	}
	return out
}

func (p *intentResourceTracker) forgetWaitingTask(taskID string) {
	for id := range p.pending(taskID) {
		p.forgetWaiting(id)
	}
}

func (p *intentResourceTracker) change() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (e *Engine) pruneResourceWaiters(t *Task) {
	pending := e.resources.pending(t.ID)
	if len(pending) == 0 {
		return
	}
	ids := make([]int64, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	nodes, err := t.Store.NodesByIDs(ids)
	if err != nil {
		return
	}
	for _, n := range nodes {
		if n.Kind == db.KindIntent && n.State == pending[n.ID] {
			delete(pending, n.ID)
		}
	}
	for id := range pending {
		e.resources.forgetWaiting(id)
	}
}

// tryClaimIntent reserves the whole lane/resource set before the state CAS.
func (e *Engine) tryClaimIntent(ctx context.Context, t *Task, in *db.Node, expected, owner string, terminal map[int64]bool) (bool, error) {
	info := intentScheduleInfoFromNode(in)
	if ok, _ := kernel.CanClaimIntent(info, nil, terminal); !ok {
		e.resources.forgetWaiting(in.ID)
		return false, nil
	}
	key, ok := e.leaseLane(t.ID, in)
	if !ok {
		e.resources.forgetWaiting(in.ID)
		return false, nil
	}
	if !e.resources.tryAcquire(resourceRequest(t.ID, in, expected)) {
		e.releaseLane(key, in.ID)
		return false, nil
	}
	claimed, err := t.Store.ClaimIntentSnapshot(ctx, in, expected, owner)
	if err != nil || !claimed {
		e.resources.release(in.ID)
		e.releaseLane(key, in.ID)
		return false, err
	}
	in.State, in.Owner = "running", owner
	return true, nil
}

func (e *Engine) awaitIntentAdmission(ctx context.Context, t *Task, in *db.Node, expected, owner string) (*db.Node, error) {
	admitted := false
	defer func() {
		if !admitted {
			e.resources.forgetWaiting(in.ID)
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		changed := e.resources.change()
		node, err := t.Store.GetNode(in.ID)
		if err != nil {
			return nil, err
		}
		if node == nil || node.State != expected || e.IsDeleting(t.ID) {
			e.resources.forgetWaiting(in.ID)
			return nil, fmt.Errorf("%w: 意图等待期间状态已变化", db.ErrIntentStateConflict)
		}
		deps, err := t.Store.TerminalIntentIDs(intentScheduleInfoFromNode(node).AfterIntents)
		if err != nil {
			return nil, err
		}
		if ok, err := e.tryClaimIntent(ctx, t, node, expected, owner, deps); err != nil {
			return nil, err
		} else if ok {
			admitted = true
			return node, nil
		}
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-changed:
		case <-time.After(800 * time.Millisecond):
		}
	}
}

func (e *Engine) releaseIntentResources(t *Task, in *db.Node) {
	e.resources.release(in.ID)
	e.releaseLane(laneKey{t.ID, agent.IntentLaneRoot(in)}, in.ID)
}
