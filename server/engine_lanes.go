package server

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
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

// recordWorkerGain preserves legacy callers while delegating to receipt-based net-gain accounting (F20).
func (e *Engine) recordWorkerGain(taskID string, state string, wrote agent.WriteCounts) {
	e.recordWorkerGainReceipt(taskID, kernel.RunReceipt{
		SettledState:    state,
		FactsWritten:    wrote.Facts,
		AssetsWritten:   wrote.Assets,
		FindingsWritten: wrote.Findings,
		ValidBatches:    wrote.ValidBatches,
		Checkpoint:      wrote.Checkpoint,
	})
}

// recordWorkerGainReceipt evaluates verified net gain from RunReceipt rather than penalizing
// exhausted partial handoffs that wrote real facts, valid batches, or checkpoints (F20).
func (e *Engine) recordWorkerGainReceipt(taskID string, receipt kernel.RunReceipt) {
	if !agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD") {
		return
	}
	count := e.gainCounter(taskID)
	if receipt.HasNetGain() {
		count.Store(0)
	} else {
		count.Add(1)
	}
}

func (e *Engine) focusWorker(taskID, name string) bool {
	return agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD") && name != "work#1" && e.gainCounter(taskID).Load() >= 4
}
