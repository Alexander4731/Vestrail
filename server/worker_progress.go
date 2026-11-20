package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"regexp"
	"sync"

	"github.com/Alexander4731/Vestrail/agent"
)

type failedToolAttempt struct{ failures, warnings int }

// Per-intent, shared across model retries, bounded memory, no command text kept.
// Success clears the corresponding failure streak; ordinary polling is unaffected.
type workerFailureGuard struct {
	mu                                     sync.Mutex
	entries                                map[[32]byte]failedToolAttempt
	order                                  [][32]byte
	cancel                                 context.CancelCauseFunc
	progress                               bool
	probeTurns, stagnantTurns, settleGrace int
	steered                                bool
	recentFPs                              [][32]byte
}

func newWorkerFailureGuard(cancel context.CancelCauseFunc) *workerFailureGuard {
	return &workerFailureGuard{entries: map[[32]byte]failedToolAttempt{}, cancel: cancel, progress: agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD")}
}

func toolAttemptKey(name string, input []byte) [32]byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			input = canonical
		}
	}
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(input)
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

func (g *workerFailureGuard) before(name string, input []byte) (bool, string) {
	key := toolAttemptKey(name, input)
	g.mu.Lock()
	if g.progress && progressWrite(name) && g.stagnantTurns >= 23 {
		g.mu.Unlock()
		return false, ""
	}
	if g.progress && progressProbe(name) {
		if g.stagnantTurns >= 23 {
			g.settleGrace++
			stop := g.settleGrace >= 3
			g.mu.Unlock()
			if stop && g.cancel != nil {
				g.cancel(agent.AbortNoProgressStagnation)
			}
			return true, "【无进展收尾】探测持续没有新信息，请立即写回已验证结论并总结结束。写回工具仍然开放。"
		}
		if g.stagnantTurns >= 15 && !g.steered {
			g.steered = true
			g.mu.Unlock()
			return true, "【进展纠偏】近期探测结果重复，请验证前提、换一种有依据的方法，或写回已有结论后结束。本次探测未执行。"
		}
	}
	entry := g.entries[key]
	if entry.failures < 3 {
		g.mu.Unlock()
		return false, ""
	}
	entry.warnings++
	g.entries[key] = entry
	stop := entry.warnings >= 3
	g.mu.Unlock()
	if stop && g.cancel != nil {
		g.cancel(agent.AbortRepeatedToolFailure)
	}
	return true, "【重复失败纠偏】同一工具及参数已经失败三次，本次未执行。请读取已有结果、验证缺失前提、采用不同方法，或写回真实失败依据并交回规划者；不要重复同一操作。"
}

func (g *workerFailureGuard) after(name string, input []byte, isError bool, result ...[]byte) {
	key := toolAttemptKey(name, input)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.progress && len(result) > 0 {
		if progressWrite(name) && !isError && meaningfulWrite(result[0]) {
			g.stagnantTurns = 0
			g.steered = false
			g.settleGrace = 0
		}
		if progressProbe(name) {
			g.probeTurns++
			fp := responseFingerprint(name, result[0], isError)
			seen := false
			for _, old := range g.recentFPs {
				if old == fp {
					seen = true
					break
				}
			}
			if g.probeTurns > 25 {
				if !isError && !seen {
					g.stagnantTurns = 0
					g.steered = false
					g.settleGrace = 0
				} else {
					g.stagnantTurns++
				}
			}
			if !seen {
				if len(g.recentFPs) == 12 {
					g.recentFPs = g.recentFPs[1:]
				}
				g.recentFPs = append(g.recentFPs, fp)
			}
		}
	}
	entry, exists := g.entries[key]
	if !isError {
		// Retain its bounded slot but clear counts, so a success permits later calls.
		if exists {
			g.entries[key] = failedToolAttempt{}
		}
		return
	}
	if !exists {
		if len(g.order) == 32 {
			delete(g.entries, g.order[0])
			g.order = g.order[1:]
		}
		g.order = append(g.order, key)
	}
	entry.failures++
	g.entries[key] = entry
}

func progressProbe(name string) bool {
	return name == "Bash" || name == "RunScript" || name == "WebFetch"
}
func progressWrite(name string) bool {
	return name == "record_fact" || name == "report_finding" || name == "insert_assets"
}
func meaningfulWrite(result []byte) bool {
	var data map[string]any
	if json.Unmarshal(result, &data) != nil {
		return len(bytes.TrimSpace(result)) > 0
	}
	if results, ok := data["results"].([]any); ok {
		for _, value := range results {
			if item, ok := value.(map[string]any); ok {
				if id, ok := item["id"].(float64); ok && id > 0 {
					return true
				}
			}
		}
		return false
	}
	if ids, ok := data["ids"].([]any); ok {
		for _, id := range ids {
			if n, ok := id.(float64); ok && n > 0 {
				return true
			}
		}
		return false
	}
	for _, key := range []string{"inserted", "added", "created"} {
		if n, ok := data[key].(float64); ok {
			return n > 0
		}
	}
	return len(data) > 0
}

var fpUUID = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
var fpTime = regexp.MustCompile(`\b(?:[0-9]{4}-[0-9]{2}-[0-9]{2}[T ]|)[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:?[0-9]{2})?`)
var fpHex = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
var fpDigits = regexp.MustCompile(`[0-9]{4,}`)

func normalizedResponse(data []byte) []byte {
	data = fpUUID.ReplaceAll(data, []byte("<uuid>"))
	data = fpTime.ReplaceAll(data, []byte("<time>"))
	data = fpHex.ReplaceAll(data, []byte("<hex>"))
	return fpDigits.ReplaceAll(data, []byte("<number>"))
}
func responseFingerprint(name string, result []byte, isError bool) [32]byte {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	if isError {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	bucket := uint64(1)
	for bucket < uint64(len(result)) {
		bucket <<= 1
	}
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], bucket)
	h.Write(size[:])
	// Normalize before taking edges, so variable UUID length cannot shift the sampled text.
	clean := normalizedResponse(result)
	h.Write(clean[:min(256, len(clean))])
	h.Write([]byte{0})
	if len(clean) > 256 {
		h.Write(clean[max(256, len(clean)-128):])
	}
	var fp [32]byte
	copy(fp[:], h.Sum(nil))
	return fp
}
