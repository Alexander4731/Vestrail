package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Alexander4731/Vestrail/agent"
	"github.com/Alexander4731/Vestrail/kernel"
)

type failedToolAttempt struct{ failures, warnings int }

// Per-intent, shared across model retries, bounded memory, no command text kept.
// Combines three orthogonal guards:
//   - Track A: repeated identical tool failures (3 warn + 3 stop)
//   - Track B: semantic stagnation (EvidenceBatch-first, input-aware fingerprint without digit wiping) (F18/F19)
//   - Track C: optional per-intent max_tool_calls enforcement with write-back reserve (F17)
type workerFailureGuard struct {
	mu                                     sync.Mutex
	entries                                map[[32]byte]failedToolAttempt
	order                                  [][32]byte
	cancel                                 context.CancelCauseFunc
	progress                               bool
	probeTurns, stagnantTurns, settleGrace int
	steered                                bool
	recentFPs                              [][32]byte
	coverage                               *kernel.CoverageTracker
	coveredDelta                           int64
	baselineBatches                        []kernel.EvidenceBatch
	validBatches                           []kernel.EvidenceBatch
	settlementRequester                    func(kernel.TerminationCode)
	requestedCode                          kernel.TerminationCode

	// Per-intent tool budget (F17): 0 = unlimited. Write-back tools are never counted or blocked.
	maxProbeTools  int
	probeToolCount int
	totalToolCalls int
}

func newWorkerFailureGuard(cancel context.CancelCauseFunc) *workerFailureGuard {
	return newWorkerFailureGuardWithBudget(cancel, 0)
}

func newWorkerFailureGuardWithBudget(cancel context.CancelCauseFunc, maxProbeTools int) *workerFailureGuard {
	return &workerFailureGuard{
		entries:       map[[32]byte]failedToolAttempt{},
		coverage:      kernel.NewCoverageTracker(""),
		cancel:        cancel,
		progress:      agent.OptimizationEnabled("VESTRAIL_OPT_PROGRESS_GUARD"),
		maxProbeTools: max(0, maxProbeTools),
	}
}

func (g *workerFailureGuard) seedCoverage(scope string, batches []kernel.EvidenceBatch) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.coverage = kernel.NewCoverageTracker(scope)
	g.coverage.Seed(batches)
	g.baselineBatches = append([]kernel.EvidenceBatch(nil), batches...)
	g.coveredDelta = 0
}

func (g *workerFailureGuard) addInputCoverage(batches []kernel.EvidenceBatch) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, batch := range batches {
		if g.coverage.Add(batch) > 0 {
			g.baselineBatches = append(g.baselineBatches, batch)
		}
	}
}

func (g *workerFailureGuard) evidenceBatches(baselineOnly bool) []kernel.EvidenceBatch {
	g.mu.Lock()
	defer g.mu.Unlock()
	batches := append([]kernel.EvidenceBatch(nil), g.baselineBatches...)
	if !baselineOnly {
		batches = append(batches, g.validBatches...)
	}
	return batches
}

func (g *workerFailureGuard) coverageDelta() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.coveredDelta
}

func (g *workerFailureGuard) setSettlementRequester(fn func(kernel.TerminationCode)) {
	g.mu.Lock()
	g.settlementRequester = fn
	g.mu.Unlock()
}

func (g *workerFailureGuard) requestedTerminationCode() kernel.TerminationCode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requestedCode
}

func (g *workerFailureGuard) requestSettlement(code kernel.TerminationCode) {
	g.mu.Lock()
	if g.requestedCode != "" {
		g.mu.Unlock()
		return
	}
	g.requestedCode = code
	fn := g.settlementRequester
	g.mu.Unlock()
	if fn != nil {
		fn(code)
	} else if g.cancel != nil {
		switch code {
		case kernel.TermNoProgress:
			g.cancel(agent.AbortNoProgressStagnation)
		case kernel.TermRepeatedToolFailure:
			g.cancel(agent.AbortRepeatedToolFailure)
		}
	}
}

func (g *workerFailureGuard) stats() (toolCalls int, batches []kernel.EvidenceBatch) {
	if g == nil {
		return 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.validBatches) > 0 {
		batches = append([]kernel.EvidenceBatch(nil), g.validBatches...)
	}
	return g.totalToolCalls, batches
}

func canonicalInputBytes(input []byte) []byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			return canonical
		}
	}
	return input
}

func toolAttemptKey(name string, input []byte) [32]byte {
	canon := canonicalInputBytes(input)
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(canon)
	var key [32]byte
	copy(key[:], h.Sum(nil))
	return key
}

func (g *workerFailureGuard) before(name string, input []byte) (bool, string) {
	key := toolAttemptKey(name, input)
	g.mu.Lock()
	// Write-back tools are always permitted so conclusions and checkpoints are never lost.
	if progressWrite(name) {
		g.mu.Unlock()
		return false, ""
	}
	// Track C: per-intent explicit tool budget (F17).
	if g.maxProbeTools > 0 && g.probeToolCount >= g.maxProbeTools {
		g.mu.Unlock()
		g.requestSettlement(kernel.TermBudgetRun)
		return true, fmt.Sprintf("【工具预算已达上限】本意图限定最多 %d 次探测/操作工具调用，当前已用完。请立即使用 record_fact / report_finding / insert_assets 写回已有结论与断点并总结结束。", g.maxProbeTools)
	}
	// Track B: semantic stagnation guard.
	if g.progress && progressProbe(name) {
		if g.stagnantTurns >= 23 {
			g.settleGrace++
			stop := g.settleGrace >= 3
			g.mu.Unlock()
			if stop {
				g.requestSettlement(kernel.TermNoProgress)
			}
			return true, "【无进展收尾】探测持续没有新信息，请立即写回已验证结论并总结结束。写回工具仍然开放。"
		}
		if g.stagnantTurns >= 15 && !g.steered {
			g.steered = true
			g.mu.Unlock()
			return true, "【进展纠偏】近期探测结果重复，请验证前提、换一种有依据的方法，或写回已有结论后结束。本次探测未执行。"
		}
	}
	// Track A: repeated identical tool failure guard.
	entry := g.entries[key]
	if entry.failures < 3 {
		g.probeToolCount++
		g.mu.Unlock()
		return false, ""
	}
	entry.warnings++
	g.entries[key] = entry
	stop := entry.warnings >= 3
	g.mu.Unlock()
	if stop {
		g.requestSettlement(kernel.TermRepeatedToolFailure)
	}
	return true, "【重复失败纠偏】同一工具及参数已经失败三次，本次未执行。请读取已有结果、验证缺失前提、采用不同方法，或写回真实失败依据并交回规划者；不要重复同一操作。"
}

func (g *workerFailureGuard) after(name string, input []byte, isError bool, result ...[]byte) {
	key := toolAttemptKey(name, input)
	request := false
	g.mu.Lock()
	defer func() {
		g.mu.Unlock()
		if request {
			g.requestSettlement(kernel.TermNoProgress)
		}
	}()
	g.totalToolCalls++
	if len(result) > 0 {
		unwrapped := unwrapToolResult(result[0])
		if g.progress && progressWrite(name) && !isError && meaningfulWrite(unwrapped) {
			g.stagnantTurns = 0
			g.steered = false
			g.settleGrace = 0
		}
		if g.progress && progressWrite(name) && name != "finish_work" && (isError || !meaningfulWrite(unwrapped)) {
			g.stagnantTurns++
			if g.stagnantTurns >= 23 && g.requestedCode == "" {
				request = true
			}
		}
		if progressProbe(name) {
			g.probeTurns++
			batches := kernel.ExtractEvidenceBatches(string(unwrapped))
			if len(batches) > 0 {
				// Structured EvidenceBatch path (F19/F27): valid unseen coverage resets stagnation;
				// invalid batches (valid=false or precondition_ok=false) never count as progress.
				newValidCoverage := false
				for _, b := range batches {
					if err := kernel.ValidateBatch(b); err != nil {
						continue
					}
					if !isError {
						if delta := g.coverage.Add(b); delta > 0 {
							g.coveredDelta += delta
							g.validBatches = append(g.validBatches, b)
							newValidCoverage = true
						}
					}
				}
				if g.progress && !isError && newValidCoverage {
					g.stagnantTurns = 0
					g.steered = false
					g.settleGrace = 0
				} else if g.progress && g.probeTurns > 25 {
					g.stagnantTurns++
				}
			} else if g.progress {
				fp := responseFingerprint(name, unwrapped, isError)
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
	return name == "record_fact" || name == "report_finding" || name == "insert_assets" || name == "finish_work"
}

// unwrapToolResult unwraps the JSON string wrapper added by norma/harness PostToolUse
// (`json.Marshal(res.Flatten())`) so downstream parsers see the real tool output (F18).
func unwrapToolResult(raw []byte) []byte {
	var unwrapped string
	if json.Unmarshal(raw, &unwrapped) == nil {
		return []byte(unwrapped)
	}
	return raw
}

var recordedTextIDRE = regexp.MustCompile(`^(?:fact|finding)\s+recorded:\s*([1-9][0-9]*)`)

// meaningfulWrite returns true ONLY when a write tool actually persisted at least
// one new/valid object (F18). Empty arrays/zero counts/all-error batches return false.
func meaningfulWrite(result []byte) bool {
	raw := bytes.TrimSpace(unwrapToolResult(result))
	if len(raw) == 0 {
		return false
	}
	jsonRaw := raw
	if index := bytes.IndexByte(raw, '{'); index > 0 {
		jsonRaw = raw[index:]
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(jsonRaw, &envelope) == nil {
		if value, ok := envelope["first_written_this_run"]; ok {
			var ids []int64
			return json.Unmarshal(value, &ids) == nil && len(ids) > 0
		}
		for _, key := range []string{"research_gain", "gain"} {
			if value, ok := envelope[key]; ok {
				var gain kernel.ResearchGain
				return json.Unmarshal(value, &gain) == nil && (gain.Evidence > 0 || gain.Coverage > 0 || gain.Artifacts > 0)
			}
		}
	}
	if recordedTextIDRE.Match(raw) {
		return true
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return false
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
	for _, key := range []string{"inserted", "added", "created", "id", "finding_node_id", "node_id"} {
		if n, ok := data[key].(float64); ok {
			return n > 0
		}
	}
	return false
}

// normalizedResponse strips volatile timestamps, UUIDs, long hex tokens, latency jitter,
// and >=8-digit nonces/epoch numbers while PRESERVING 1–7 digit numbers (ports, ranges,
// batch offsets, counts) so distinct negative coverage slices never collide (F19).
func normalizedResponse(data []byte) []byte {
	return kernel.NormalizeObservation(data)
}

func responseFingerprint(name string, result []byte, isError bool) [32]byte {
	return responseFingerprintWithInput(name, nil, result, isError)
}

// responseFingerprintWithInput hashes tool name, error flag, normalized input signature,
// and normalized full output so distinct valid negative ranges do not collide
// while repeated noise with changing timestamps still matches (F19).
func responseFingerprintWithInput(name string, input []byte, result []byte, isError bool) [32]byte {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	if isError {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write([]byte{0})
	if len(input) > 0 {
		h.Write(normalizedResponse(canonicalInputBytes(input)))
	}
	h.Write([]byte{0})
	clean := normalizedResponse(bytes.TrimSpace(result))
	// Hash full normalized output (capped only against extreme multi-MB blobs) so
	// mid-log result differences are not ignored.
	const maxHashBytes = 64 * 1024
	if len(clean) > maxHashBytes {
		h.Write(clean[:maxHashBytes/2])
		h.Write([]byte{0})
		h.Write(clean[len(clean)-maxHashBytes/2:])
	} else {
		h.Write(clean)
	}
	var fp [32]byte
	copy(fp[:], h.Sum(nil))
	return fp
}

func formatReceiptSummary(r kernel.RunReceipt) string {
	return strings.TrimSpace(fmt.Sprintf("state=%s term=%s delivery=%s tools=%d facts=%d assets=%d findings=%d batches=%d",
		r.SettledState, r.TerminationCode, r.DeliveryStatus, r.ToolCalls, r.FactsWritten, r.AssetsWritten, r.FindingsWritten, len(r.ValidBatches)))
}
