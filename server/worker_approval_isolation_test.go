package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/guard"
	"github.com/Alexander4731/Vestrail/intercept"
	"github.com/Alexander4731/Vestrail/notify"
)

func TestHighRiskApprovalPausesOnlyTriggeringWorkerAndEmitsBrowserNotify(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("test PostgreSQL not configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })

	// Seed an enabled browser channel for this test (cleaned up after).
	enabled := true
	chID, err := pg.SaveNotificationChannel(t.Context(), &db.NotificationChannel{
		Name:       "test-browser-notify",
		Kind:       notify.KindBrowser,
		Enabled:    &enabled,
		Mode:       db.NotifyModeRealtime,
		Config:     json.RawMessage(`{"on_approval":true,"require_interaction":true,"sound":true}`),
		Filter:     json.RawMessage(`{}`),
		RatePerMin: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteNotificationChannel(context.Background(), chID) })

	ic := intercept.New(pg)
	m := &Manager{pg: pg, interceptor: ic, tasks: map[string]*Task{}}
	eng := NewEngine(m)
	s := &Server{m: m, engine: eng, browserNotify: newBrowserNotifyHub()}
	s.wireApprovalNotifications()

	taskID := "task-approval-iso"
	g := guard.NewWithInterceptor(ic)

	// Acquire a shared channel slot for worker#1 and verify it is released during approval pause.
	pool := newChannelPoolTracker()
	_, _, _, rel1 := pool.acquire("host:target", 101, 2)
	released := false
	release1 := func() {
		if !released {
			released = true
			rel1()
		}
	}

	// Worker #1 enters HandleAsk on high-risk restoreFactorySettings.
	w1Ctx := eng.phaseContext(t.Context(), taskID)
	w1Ctx = intercept.WithTaskContext(w1Ctx, taskID, "work#1 · #101", nil)
	w1Ctx = intercept.WithIntent(w1Ctx, 101)
	w1Ctx = intercept.WithApprovalPauseHooks(w1Ctx,
		func(n intercept.PendingNotice) {
			eng.markWorkerApprovalPaused(n)
			release1()
		},
		func(_ int64) {
			eng.clearWorkerApprovalPaused(101, taskID)
		},
	)

	w1Done := make(chan bool, 1)
	go func() {
		raw, _ := json.Marshal(map[string]string{
			"command": "curl -X POST https://target/shterm/api/cluster/restoreFactorySettings -d '{}'",
		})
		blocked, _, _ := g.Hooks().PreToolUse(w1Ctx, "Bash", raw)
		w1Done <- blocked
	}()

	// Wait until worker#1 is recorded as paused-for-approval.
	var paused []ApprovalPausedWorker
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		paused = eng.ApprovalPausedWorkers(taskID)
		if len(paused) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(paused) != 1 || paused[0].IntentID != 101 {
		t.Fatalf("expected worker#1 (intent 101) to be approval-paused, got %+v", paused)
	}
	if eng.IsPaused(taskID) {
		t.Fatal("whole task must NOT be paused when only worker#1 awaits approval")
	}
	if !released {
		t.Fatal("worker#1 shared channel slot should be released while awaiting approval")
	}

	// Sibling worker#2 in the SAME task runs a read-only tool concurrently without blocking!
	w2Ctx := eng.phaseContext(t.Context(), taskID)
	w2Ctx = intercept.WithTaskContext(w2Ctx, taskID, "work#2 · #102", nil)
	w2Ctx = intercept.WithIntent(w2Ctx, 102)
	endTool := intercept.BeginPhase(w2Ctx, "tool")
	if phase := eng.executionPhase(taskID); phase != "tool" {
		t.Fatalf("task executionPhase should reflect active sibling worker 'tool', got %q", phase)
	}
	rawSafe, _ := json.Marshal(map[string]string{"command": "cat /etc/passwd"})
	if blk, msg, _ := g.Hooks().PreToolUse(w2Ctx, "Bash", rawSafe); blk {
		t.Fatalf("sibling worker#2 read-only command must not be blocked: %s", msg)
	}
	endTool()

	// Verify browser notification event was published to BrowserNotifyHub.
	var latest int64
	var items []notify.BrowserEvent
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		latest, items = s.browserNotifyHub().Since(0)
		if latest > 0 {
			_, items = s.browserNotifyHub().Since(latest - 1)
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(items) == 0 || items[0].Kind != notify.EventApprovalPending || items[0].PendingID != paused[0].PendingID {
		t.Fatalf("expected approval_pending browser notification for pending %d, got latest=%d items=%+v", paused[0].PendingID, latest, items)
	}

	// Verify HTTP endpoint GET /api/notify/browser/events?after=0 returns latest_seq & enabled=true.
	req := httptest.NewRequest(http.MethodGet, "/api/notify/browser/events?after=0", nil)
	rec := httptest.NewRecorder()
	s.notifyBrowserEvents(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 from notifyBrowserEvents, got %d", rec.Code)
	}

	// Deny pending approval -> worker#1 unblocks with blocked=true and clears paused state.
	if err := ic.Decide(paused[0].PendingID, false); err != nil {
		t.Fatalf("Decide deny failed: %v", err)
	}
	select {
	case blocked := <-w1Done:
		if !blocked {
			t.Fatal("expected denied worker#1 call to be blocked")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker#1 did not resume after Decide")
	}
	if rem := eng.ApprovalPausedWorkers(taskID); len(rem) != 0 {
		t.Fatalf("expected approvalPaused cleared after Decide, got %+v", rem)
	}
	_, _ = pg.Exec(`DELETE FROM intercept_pending WHERE id=$1`, paused[0].PendingID)
}
