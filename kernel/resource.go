package kernel

import (
	"fmt"
	"strings"
)

// IntentScheduleInfo summarizes one intent's dependency and resource-lock requirements.
type IntentScheduleInfo struct {
	ID            int64
	State         string
	AfterIntents  []int64
	ResourceLocks []ResourceLock
}

// CanClaimIntent enforces prerequisite intent completion and reader-writer resource locks (F22).
// AfterIntents block only while the prerequisite remains active.
// Resource ownership belongs to running/reserved work. Queue ordering is
// expressed by AfterIntents, not by treating open intents as lock holders.
func CanClaimIntent(candidate IntentScheduleInfo, active []IntentScheduleInfo, activeIntentIDs map[int64]bool) (bool, string) {
	for _, depID := range candidate.AfterIntents {
		if depID <= 0 || depID == candidate.ID {
			continue
		}
		if activeIntentIDs[depID] {
			return false, fmt.Sprintf("等待前置意图 #%d 结束", depID)
		}
	}
	if len(candidate.ResourceLocks) == 0 {
		return true, ""
	}
	for _, want := range candidate.ResourceLocks {
		res := strings.TrimSpace(strings.ToLower(want.Resource))
		if res == "" {
			continue
		}
		wantExclusive := strings.EqualFold(strings.TrimSpace(want.Mode), "exclusive")
		for _, other := range active {
			if other.ID == candidate.ID {
				continue
			}
			if other.State != "running" {
				continue
			}
			for _, held := range other.ResourceLocks {
				if !strings.EqualFold(strings.TrimSpace(held.Resource), res) {
					continue
				}
				heldExclusive := strings.EqualFold(strings.TrimSpace(held.Mode), "exclusive")
				if wantExclusive {
					return false, fmt.Sprintf("资源 %s 正被意图 #%d(%s) 占用，等待其完成后再独占执行", want.Resource, other.ID, other.State)
				}
				if heldExclusive && other.State == "running" {
					return false, fmt.Sprintf("资源 %s 正被意图 #%d 独占执行", want.Resource, other.ID)
				}
			}
		}
	}
	return true, ""
}

// DefaultSharedChannelCap is the default total concurrency budget across workers
// sharing a single proxy/channel pool (F21).
const DefaultSharedChannelCap = 8

// WorkerConcurrencyBudget splits a shared pool's total concurrency budget across
// active workers, floored at 1.
func WorkerConcurrencyBudget(poolCap, activeHolders int) int {
	if poolCap <= 0 {
		poolCap = DefaultSharedChannelCap
	}
	if activeHolders <= 1 {
		return poolCap
	}
	budget := poolCap / activeHolders
	if budget < 1 {
		return 1
	}
	return budget
}
