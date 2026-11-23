package server

import (
	"context"
	"time"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/db"
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
