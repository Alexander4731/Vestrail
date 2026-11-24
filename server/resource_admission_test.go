package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/llm"
)

func TestExclusiveResourceHasOneConcurrentOwner(t *testing.T) {
	p := newIntentResourceTracker()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var owners atomic.Int32
	for id := int64(1); id <= 20; id++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			if p.tryAcquire(intentResourceRequest{intentID: id, taskID: "task", priority: 5, locks: map[string]bool{"resource": true}}, true) {
				owners.Add(1)
			}
		}(id)
	}
	close(start)
	wg.Wait()
	if owners.Load() != 1 {
		t.Fatalf("owners=%d", owners.Load())
	}
}

func TestResourceClaimFindsRunnableIntentAfterBlockedPage(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	e := NewEngine(m)
	for i := 0; i < 33; i++ {
		if _, err := task.Store.AddIntent(map[string]any{"summary": fmt.Sprintf("blocked %d", i), "resource_locks": []kernel.ResourceLock{{Resource: "channel:fixture", Mode: "exclusive"}}}, 10, nil, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if !e.resources.tryAcquire(intentResourceRequest{intentID: -1, taskID: "other", locks: map[string]bool{"channel:fixture": true}}, true) {
		t.Fatal("fixture reservation failed")
	}
	defer e.resources.release(-1)
	id, err := task.Store.AddIntent(map[string]any{"summary": "runnable on another resource"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	claimed := e.claimNextContext(t.Context(), task, "worker")
	if claimed == nil || claimed.ID != id {
		t.Fatalf("claimed=%+v want=%d", claimed, id)
	}
	defer e.releaseIntentResources(task, claimed)
}

func TestTaskDrainHonorsCapturedRunDeadline(t *testing.T) {
	e := &Engine{}
	buffer := 180
	plan := kernel.ComputeBudgetPlanWithPolicy(30, 0, time.Now(), kernel.BudgetPolicy{HardBufferSeconds: &buffer})
	hard := time.Now().Add(plan.HardDuration())
	key := &activeRunDeadline{taskID: "fixture"}
	e.runDeadlines.Store(key, hard)
	watchdog := time.Now().Add(settleDrainGrace)
	if got := e.taskDrainDeadline("fixture", watchdog); got.Before(hard) {
		t.Fatal("configured hard buffer truncated")
	}
	if got := e.taskDrainDeadline("another", watchdog); !got.Equal(watchdog) {
		t.Fatal("another task extended drain")
	}
}

func TestConcurrentResourceAndStateClaimHasOneWinner(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "local fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	id, err := task.Store.AddIntent(map[string]any{"summary": "concurrent claim", "resource_locks": []kernel.ResourceLock{{Resource: "fixture", Mode: "exclusive"}}}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	node, err := task.Store.GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(m)
	start := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			copy := *node
			<-start
			won, err := e.tryClaimIntent(t.Context(), task, &copy, "open", fmt.Sprint(i), nil, true)
			if err != nil && !errors.Is(err, errIntentResourcesBusy) {
				t.Error(err)
			}
			if won {
				winners.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	defer e.releaseIntentResources(task, node)
	if winners.Load() != 1 {
		t.Fatalf("winners=%d", winners.Load())
	}
	node, err = task.Store.GetNode(id)
	if err != nil || node.State != "running" {
		t.Fatalf("node=%+v err=%v", node, err)
	}
}

func TestAdHocDispatchRunsOneIntentWithoutRevivingOldQueue(t *testing.T) {
	oldAugment, oldResolve, oldPrompt := agent.ToolAugment, agent.ToolResolve, agent.PromptOverride
	agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = nil, nil, nil
	t.Cleanup(func() { agent.ToolAugment, agent.ToolResolve, agent.PromptOverride = oldAugment, oldResolve, oldPrompt })
	for _, mode := range []string{"paused", "done"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTask(t.Name(), "local fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer m.DeleteTask(task.ID, DeleteTaskOptions{})
			old, err := task.Store.AddIntent(map[string]any{"summary": "old queued intent"}, 5, nil, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "paused" {
				if err := m.SetTaskPaused(task.ID, true); err != nil {
					t.Fatal(err)
				}
			} else if err := m.SetTaskStatus(task.ID, "done"); err != nil {
				t.Fatal(err)
			}
			before := task.lifecycleSnapshot()
			e := NewEngine(m)
			if mode == "paused" {
				e.Pause(task.ID, agent.AbortPausedByUser)
			}
			e.deadline.Store(task.ID, time.Now().Add(-time.Hour).Unix())
			var calls atomic.Int32
			provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if calls.Add(1) == 1 {
					input := json.RawMessage(`{"delivery_status":"partial_checkpoint","summary":"local single dispatch completed","checkpoint":{"remaining_scope":"secondary verification","next_action":"review fixture"}}`)
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "finish", Name: "finish_work", Input: input}}}, "tool_use", llm.Usage{}, nil
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("Local fixture checkpoint.")}}, "end_turn", llm.Usage{}, nil
			}}
			worker := agent.NewWorker(provider, "fixture", m.dir, nil, 32000, 5)
			worker.SetNonStreaming(func() bool { return true })
			e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, worker })
			id, standalone, err := task.Store.AddAdHocIntentLinked(map[string]any{"summary": "single fixture"}, 5, "human", nil, nil, db.RelYields)
			if err != nil || !standalone {
				t.Fatalf("standalone=%v err=%v", standalone, err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := e.RunAdHocIntent(ctx, task, id); err != nil {
				t.Fatal(err)
			}
			for e.taskOperationCount(task.ID) > 0 && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
			}
			if ctx.Err() != nil {
				t.Fatal("single dispatch did not settle")
			}
			node, err := task.Store.GetNode(id)
			previous, _ := task.Store.GetNode(old)
			after := task.lifecycleSnapshot()
			if err != nil || node.State != "exhausted" || previous.State != "open" || before.Status != after.Status || before.Paused != after.Paused || calls.Load() != 1 {
				t.Fatalf("new=%+v old=%+v calls=%d lifecycle=%+v err=%v", node, previous, calls.Load(), after, err)
			}
		})
	}
}

func TestWaitingWriterIsNotAHolderAndCanBeCancelled(t *testing.T) {
	p := newIntentResourceTracker()
	reader := intentResourceRequest{intentID: 1, taskID: "a", priority: 5, locks: map[string]bool{"resource": false}}
	writer := intentResourceRequest{intentID: 2, taskID: "b", priority: 5, locks: map[string]bool{"resource": true}}
	later := intentResourceRequest{intentID: 3, taskID: "c", priority: 5, locks: map[string]bool{"resource": false}}
	if !p.tryAcquire(reader, true) || p.tryAcquire(writer, true) || p.tryAcquire(later, true) {
		t.Fatal("unexpected admission order")
	}
	p.forgetWaiting(writer.intentID)
	if !p.tryAcquire(later, true) {
		t.Fatal("cancelled writer retained queue ownership")
	}
	p.release(reader.intentID)
	p.release(later.intentID)
	if !p.tryAcquire(writer, true) {
		t.Fatal("writer could not acquire released resource")
	}
	p.forgetWaiting(writer.intentID)
	if p.tryAcquire(later, true) {
		t.Fatal("forgetWaiting released an active holder")
	}
}

func TestResourceTrackerCrossTaskIsolation(t *testing.T) {
	tracker := newIntentResourceTracker()
	node := func(id int64, resource string) *db.Node {
		raw, _ := json.Marshal(map[string]any{"resource_locks": []kernel.ResourceLock{{Resource: resource, Mode: "exclusive"}}})
		return &db.Node{ID: id, Payload: raw}
	}
	if !tracker.tryAcquire(resourceRequest("task-a", node(1, "logical-key"), "open"), false) || !tracker.tryAcquire(resourceRequest("task-b", node(2, "logical-key"), "open"), false) {
		t.Fatal("logical locks crossed task boundary")
	}
	if !tracker.tryAcquire(resourceRequest("task-a", node(3, "channel:shared"), "open"), false) || tracker.tryAcquire(resourceRequest("task-b", node(4, "channel:shared"), "open"), false) {
		t.Fatal("shared channel isolation failed")
	}
}

func TestResourceTrackerNoGhostWaiterAfterTaskDone(t *testing.T) {
	tracker := newIntentResourceTracker()
	holder := intentResourceRequest{taskID: "holder", intentID: 1, locks: map[string]bool{"channel:shared": true}}
	blocked := intentResourceRequest{taskID: "finished", intentID: 2, priority: 10, locks: holder.locks}
	if !tracker.tryAcquire(holder, false) || tracker.tryAcquire(blocked, false) {
		t.Fatal("fixture admission invalid")
	}
	if len(tracker.pending("finished")) != 0 {
		t.Fatal("nonblocking probe left a waiter")
	}
	if tracker.tryAcquire(blocked, true) {
		t.Fatal("held channel admitted waiter")
	}
	tracker.forgetWaitingTask("finished")
	tracker.release(1)
	blocked.intentID, blocked.priority = 3, 0
	if !tracker.tryAcquire(blocked, false) {
		t.Fatal("finished task starved later intent")
	}
}

func TestResourceTrackerBusyLaneDoesNotStarveOtherLane(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	engine := NewEngine(m)
	first, err := task.Store.AddIntent(map[string]any{"summary": "busy lane", "resource_locks": []kernel.ResourceLock{{Resource: "fixture", Mode: "exclusive"}}}, 10, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	second, err := task.Store.AddIntent(map[string]any{"summary": "runnable lane", "resource_locks": []kernel.ResourceLock{{Resource: "fixture", Mode: "exclusive"}}}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	engine.laneBusy.Store(laneKey{taskID: task.ID, root: first}, int64(-1))
	claimed := engine.claimNextContext(t.Context(), task, "worker")
	if claimed == nil || claimed.ID != second {
		t.Fatalf("claimed=%+v wanted=%d", claimed, second)
	}
	engine.releaseIntentResources(task, claimed)
	if len(engine.resources.pending(task.ID)) != 0 {
		t.Fatal("busy lane left a global waiter")
	}
}
