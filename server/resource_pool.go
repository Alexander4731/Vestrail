package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
)

type channelPoolTracker struct {
	mu     sync.Mutex
	active map[string]map[int64]struct{} // poolKey -> set of active intent IDs
}

func newChannelPoolTracker() *channelPoolTracker {
	return &channelPoolTracker{active: map[string]map[int64]struct{}{}}
}

func sharedChannelCap() int {
	if raw := strings.TrimSpace(os.Getenv("VESTRAIL_SHARED_CHANNEL_CAP")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			return v
		}
	}
	return kernel.DefaultSharedChannelCap
}

// acquire registers intentID on poolKey and returns the total cap, active worker
// count, and per-worker concurrency budget together with a release closure (F21).
func (p *channelPoolTracker) acquire(poolKey string, intentID int64, configuredWorkers ...int) (capTotal, activeCount, workerBudget int, release func()) {
	if p == nil || strings.TrimSpace(poolKey) == "" {
		return 0, 0, 0, func() {}
	}
	p.mu.Lock()
	holders := p.active[poolKey]
	if holders == nil {
		holders = map[int64]struct{}{}
		p.active[poolKey] = holders
	}
	holders[intentID] = struct{}{}
	activeCount = len(holders)
	p.mu.Unlock()

	capTotal = sharedChannelCap()
	workers := activeCount
	if len(configuredWorkers) > 0 {
		workers = max(workers, configuredWorkers[0])
	}
	workerBudget = kernel.WorkerConcurrencyBudget(capTotal, workers)
	var once sync.Once
	release = func() {
		once.Do(func() {
			p.mu.Lock()
			if h := p.active[poolKey]; h != nil {
				delete(h, intentID)
				if len(h) == 0 {
					delete(p.active, poolKey)
				}
			}
			p.mu.Unlock()
		})
	}
	return capTotal, activeCount, workerBudget, release
}

func (p *channelPoolTracker) poolKeyFor(taskID, proxyAddr string, intent *db.Node) string {
	info := intentScheduleInfoFromNode(intent)
	for _, lock := range info.ResourceLocks {
		res := strings.TrimSpace(strings.ToLower(lock.Resource))
		if strings.HasPrefix(res, "channel:") || strings.HasPrefix(res, "proxy:") {
			return res
		}
	}
	if strings.TrimSpace(proxyAddr) != "" {
		return "proxy:" + strings.TrimSpace(proxyAddr)
	}
	return "task:" + taskID
}

func channelBudgetEnv(capTotal, activeCount, workerBudget int) []string {
	if workerBudget <= 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("VESTRAIL_CHANNEL_TOTAL_BUDGET=%d", capTotal),
		fmt.Sprintf("VESTRAIL_CHANNEL_ACTIVE_WORKERS=%d", activeCount),
		fmt.Sprintf("VESTRAIL_WORKER_CONCURRENCY_BUDGET=%d", workerBudget),
	}
}

func intentScheduleInfoFromNode(n *db.Node) kernel.IntentScheduleInfo {
	if n == nil {
		return kernel.IntentScheduleInfo{}
	}
	info := kernel.IntentScheduleInfo{ID: n.ID, State: n.State}
	var p struct {
		AfterIntents  []int64               `json:"after_intents"`
		ResourceLocks []kernel.ResourceLock `json:"resource_locks"`
	}
	if json.Unmarshal(n.Payload, &p) == nil {
		info.AfterIntents = p.AfterIntents
		info.ResourceLocks = p.ResourceLocks
	}
	return info
}

func intentMaxToolCalls(n *db.Node) int {
	if n == nil {
		return 0
	}
	var p struct {
		MaxToolCalls int `json:"max_tool_calls"`
	}
	if json.Unmarshal(n.Payload, &p) == nil && p.MaxToolCalls > 0 {
		return p.MaxToolCalls
	}
	return 0
}
