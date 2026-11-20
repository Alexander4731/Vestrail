package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/Autumn-27/norma/llm"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
)

func TestProgressGuardProtectsSlowExploitAndSettlesLoop(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	g := newWorkerFailureGuard(cancel)
	for i := 0; i < 135; i++ {
		input := []byte(fmt.Sprintf(`{"step":%d}`, i))
		if blocked, _ := g.before("Bash", input); blocked {
			t.Fatalf("new evidence blocked at %d", i)
		}
		g.after("Bash", input, false, []byte(fmt.Sprintf("new distinct fact marker-%d", i)))
	}
	if g.stagnantTurns != 0 || ctx.Err() != nil {
		t.Fatal("slow progressing chain stopped")
	}
	g = newWorkerFailureGuard(cancel)
	for i := 0; i < 25; i++ {
		g.after("Bash", nil, false, []byte("fixed"))
		if g.stagnantTurns != 0 {
			t.Fatal("warm-up exemption missing")
		}
	}
	for range 15 {
		g.after("Bash", nil, false, []byte("fixed"))
	}
	if blocked, _ := g.before("Bash", nil); !blocked || ctx.Err() != nil {
		t.Fatal("soft steering not applied")
	}
	if blocked, _ := g.before("record_fact", nil); blocked {
		t.Fatal("writeback blocked during steer")
	}
	for range 8 {
		g.after("Bash", nil, false, []byte("fixed"))
	}
	if blocked, _ := g.before("report_finding", nil); blocked {
		t.Fatal("confirmed result blocked")
	}
	for i := 0; i < 3; i++ {
		if blocked, _ := g.before("RunScript", []byte(`{}`)); !blocked {
			t.Fatal("stagnant probe allowed")
		}
		if i < 2 && ctx.Err() != nil {
			t.Fatal("grace ended early")
		}
	}
	if !errors.Is(context.Cause(ctx), agent.AbortNoProgressStagnation) {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
}
func TestProgressGuardWritebackAndStatusFingerprint(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	g := newWorkerFailureGuard(nil)
	g.stagnantTurns = 23
	g.steered = true
	g.after("record_fact", nil, false, []byte(`{"ids":[0],"errors":["failed"]}`))
	if g.stagnantTurns != 23 {
		t.Fatal("empty batch reset progress")
	}
	g.after("record_fact", nil, false, []byte(`{"ids":[123]}`))
	if g.stagnantTurns != 0 || g.steered {
		t.Fatal("writeback did not reset")
	}
	a := []byte("HTTP 200 2026-11-20T12:01:02Z id=01234567-89ab-cdef-0123-456789abcdef 123456789")
	b := []byte("HTTP 200 2026-11-21T13:02:03Z id=abcdef01-2345-6789-abcd-ef0123456789 987654321")
	if responseFingerprint("Bash", a, false) != responseFingerprint("Bash", b, false) {
		t.Fatal("volatile values not normalized")
	}
	if responseFingerprint("Bash", a, false) == responseFingerprint("Bash", []byte("HTTP 403 2026-11-20T12:01:02Z id=01234567-89ab-cdef-0123-456789abcdef 123456789"), false) {
		t.Fatal("HTTP status conflated")
	}
	if responseFingerprint("Bash", a, false) == responseFingerprint("Bash", a, true) {
		t.Fatal("error flag missing")
	}
}
func TestWorkerLaneLeaseAndAdaptiveGain(t *testing.T) {
	t.Setenv("VESTRAIL_OPT_PROGRESS_GUARD", "1")
	e := NewEngine(nil)
	one := &db.Node{ID: 10, Payload: []byte(`{"lane_root_id":5}`)}
	two := &db.Node{ID: 11, Payload: []byte(`{"lane_root_id":5}`)}
	key, ok := e.leaseLane("a", one)
	if !ok {
		t.Fatal("lease failed")
	}
	if _, duplicate := e.leaseLane("a", one); duplicate {
		t.Fatal("duplicate claim stole lease")
	}
	if _, ok := e.awaitLane(t.Context(), "a", one); !ok {
		t.Fatal("registered claim lost lease")
	}
	if _, ok = e.leaseLane("a", two); ok {
		t.Fatal("same lane concurrent")
	}
	if _, ok = e.leaseLane("b", two); !ok {
		t.Fatal("task namespaces collide")
	}
	e.releaseLane(key, two.ID)
	if _, ok = e.leaseLane("a", two); ok {
		t.Fatal("non-owner released")
	}
	e.releaseLane(key, one.ID)
	if _, ok = e.leaseLane("a", two); !ok {
		t.Fatal("owner release failed")
	}
	for range 4 {
		e.recordWorkerGain("a", "exhausted", agent.WriteCounts{})
	}
	if !e.focusWorker("a", "work#2") || e.focusWorker("a", "work#1") {
		t.Fatal("focus concurrency incorrect")
	}
	e.recordWorkerGain("a", "done", agent.WriteCounts{Facts: 1})
	if e.focusWorker("a", "work#2") {
		t.Fatal("gain did not restore parallelism")
	}
}

func TestLaneClaimSkipsFullBusyFrontier(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "generated data only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	e := NewEngine(m)
	rootID, err := task.Store.AddIntent(map[string]any{"summary": "root"}, 10, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	first := e.claimNext(task, "work#1")
	if first == nil || first.ID != rootID {
		t.Fatal("nil busy array hid frontier")
	}
	defer e.releaseLane(laneKey{task.ID, rootID}, rootID)
	for i := 0; i < 25; i++ {
		if _, err := task.Store.AddIntent(map[string]any{"summary": "blocked lane", "lane_root_id": rootID}, 10, nil, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	freeID, err := task.Store.AddIntent(map[string]any{"summary": "different lane"}, 1, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	next := e.claimNext(task, "work#2")
	if next == nil || next.ID != freeID {
		t.Fatal("busy lane starved other lane beyond first 20")
	}
	defer e.releaseLane(laneKey{task.ID, freeID}, freeID)
}

func TestManualLaneWaitCanBePaused(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask(t.Name(), "generated data only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	if err := m.SetTaskPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTaskStatus(task.ID, "running"); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(m)
	rootID, err := task.Store.AddIntent(map[string]any{"summary": "root"}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := task.Store.GetNode(rootID)
	key, ok := e.leaseLane(task.ID, root)
	if !ok {
		t.Fatal("lease failed")
	}
	defer e.releaseLane(key, rootID)
	childID, err := task.Store.AddIntent(map[string]any{"summary": "manual continuation", "lane_root_id": rootID}, 5, nil, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if won, err := task.Store.ClaimIntent(childID, "manual"); err != nil || !won {
		t.Fatalf("claim %v %v", won, err)
	}
	child, _ := task.Store.GetNode(childID)
	var calls atomic.Int64
	provider := retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls.Add(1)
		return llm.Message{}, "", llm.Usage{}, fmt.Errorf("unexpected model call")
	}}
	worker := agent.NewWorker(provider, "fixture", m.dir, nil, 200000, 5)
	if !e.beginTaskOperation(task.ID) {
		t.Fatal("admission failed")
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer e.decInflight(task.ID)
		e.runIntent(t.Context(), task, "manual", worker, child, "request", "fixture message")
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.workMu.Lock()
		registered := e.work[childID] != nil
		e.workMu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting work was not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.ControlWork(t.Context(), childID, "pause"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting work did not stop")
	}
	node, err := task.Store.GetNode(childID)
	if err != nil || node.State != "paused" || calls.Load() != 0 {
		t.Fatalf("node=%+v calls=%d err=%v", node, calls.Load(), err)
	}
	if owner, ok := e.laneBusy.Load(key); !ok || owner.(int64) != rootID {
		t.Fatal("waiting child released source lease")
	}
}
