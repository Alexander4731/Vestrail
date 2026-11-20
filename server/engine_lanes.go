package server

import (
	"context"
	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"sync/atomic"
	"time"
)

type laneKey struct {
	taskID string
	root   int64
}

func (e *Engine) leaseLane(taskID string, in *db.Node) (laneKey, bool) {
	key := laneKey{taskID, agent.IntentLaneRoot(in)}
	_, busy := e.laneBusy.LoadOrStore(key, in.ID)
	return key, !busy
}
func (e *Engine) releaseLane(key laneKey, id int64) { e.laneBusy.CompareAndDelete(key, id) }
func (e *Engine) awaitLane(ctx context.Context, taskID string, in *db.Node) (laneKey, bool) {
	for {
		key := laneKey{taskID, agent.IntentLaneRoot(in)}
		if owner, ok := e.laneBusy.Load(key); ok && owner.(int64) == in.ID {
			return key, true
		}
		key, ok := e.leaseLane(taskID, in)
		if ok {
			return key, true
		}
		if sleepCtx(ctx, 100*time.Millisecond) {
			return key, false
		}
	}
}
func (e *Engine) gainCounter(taskID string) *atomic.Int64 {
	value, _ := e.noGainStreak.LoadOrStore(taskID, &atomic.Int64{})
	return value.(*atomic.Int64)
}
func (e *Engine) recordWorkerGain(taskID string, state string, wrote agent.WriteCounts) {
	if !agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD") {
		return
	}
	count := e.gainCounter(taskID)
	if wrote.Findings > 0 || (wrote.Facts > 0 && state == "done") {
		count.Store(0)
	} else if wrote.Total() == 0 || state == "exhausted" {
		count.Add(1)
	}
}
func (e *Engine) focusWorker(taskID, name string) bool {
	return agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD") && name != "work#1" && e.gainCounter(taskID).Load() >= 4
}
