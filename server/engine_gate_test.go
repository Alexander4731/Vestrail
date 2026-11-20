package server

import (
	"testing"

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
