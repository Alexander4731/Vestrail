package server

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
)

func TestOnlyDoneTriggers(t *testing.T) {
	if onlyDoneTriggers(nil) {
		t.Fatal("empty triggers (bare wake/heartbeat) must not be gated")
	}
	if !onlyDoneTriggers([]agent.TriggerEvent{{Kind: "done", IntentID: 1}, {Kind: "done", IntentID: 2}}) {
		t.Fatal("all-done trigger batch should be eligible for saturated-pool coalescing")
	}
	for _, kind := range []string{"finding", "goal", "goal_deleted", "goal_edited", "hint", "cancelled"} {
		batch := []agent.TriggerEvent{
			{Kind: "done", IntentID: 1},
			{Kind: kind, IntentID: 2},
		}
		if onlyDoneTriggers(batch) {
			t.Fatalf("high-priority trigger %q must bypass coalescing gate", kind)
		}
	}
}

func TestPlannerEmptyRoundsReachesPauseForInput(t *testing.T) {
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
	if _, err := task.Store.AddGoal(map[string]any{"text": "unmet fixture"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("no change")}}, "end_turn", llm.Usage{}, nil
	}}
	planner := agent.NewPlanner(provider, "fixture", m.dir, nil, 200000, 2)
	defer planner.Close()
	planner.SetNonStreaming(func() bool { return true })
	engine := NewEngine(m)
	engine.debounce = time.Millisecond
	engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); engine.plannerLoop(ctx, task) }()
	task.Notify()
	for !engine.IsPaused(task.ID) && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if !engine.IsPaused(task.ID) || calls.Load() != 3 {
		t.Fatalf("paused=%v calls=%d", engine.IsPaused(task.ID), calls.Load())
	}
}

func TestPlannerRetryableModelErrorBackoffFires(t *testing.T) {
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
	if _, err := task.Store.AddGoal(map[string]any{"text": "unmet fixture"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{}, "", llm.Usage{}, context.DeadlineExceeded
	}}
	planner := agent.NewPlanner(provider, "fixture", m.dir, nil, 200000, 1)
	defer planner.Close()
	planner.SetNonStreaming(func() bool { return true })
	engine := NewEngine(m)
	engine.debounce = time.Millisecond
	engine.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return planner, nil })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); engine.plannerLoop(ctx, task) }()
	task.Notify()
	for calls.Load() < 2 && ctx.Err() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() < 2 || engine.IsPaused(task.ID) {
		t.Fatalf("paused=%v calls=%d", engine.IsPaused(task.ID), calls.Load())
	}
}
