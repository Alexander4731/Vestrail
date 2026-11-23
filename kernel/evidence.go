package kernel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const BatchSchemaV1 = "vestrail.batch.v1"

// EvidenceBatch describes one machine-verifiable batch execution result (F19/F26/F27).
type EvidenceBatch struct {
	Schema         string `json:"schema"`
	BatchKey       string `json:"batch_key,omitempty"`
	Scope          string `json:"scope,omitempty"`
	RangeStart     int64  `json:"range_start,omitempty"`
	RangeEnd       int64  `json:"range_end,omitempty"`
	Valid          bool   `json:"valid"`
	PreconditionOK bool   `json:"precondition_ok"`
	Attempted      int64  `json:"attempted"`
	UniqueCovered  int64  `json:"unique_covered"`
	Positive       int64  `json:"positive"`
	ArtifactPath   string `json:"artifact_path,omitempty"`
	ArtifactDigest string `json:"artifact_digest,omitempty"`
	Provenance     string `json:"provenance,omitempty"`
	Target         string `json:"target,omitempty"`
	Method         string `json:"method,omitempty"`
	Precondition   string `json:"precondition,omitempty"`
}

// CanonicalKey identifies the covered slice so distinct valid negative batches
// never collide in progress tracking.
func (b EvidenceBatch) CanonicalKey() string {
	if k := strings.TrimSpace(b.BatchKey); k != "" {
		return k
	}
	if b.RangeStart != 0 || b.RangeEnd != 0 {
		return fmt.Sprintf("range:%d-%d", b.RangeStart, b.RangeEnd)
	}
	if s := strings.TrimSpace(b.Scope); s != "" {
		return "scope:" + s
	}
	return strings.TrimSpace(b.ArtifactPath)
}

// ValidateBatch checks inclusive-range bounds and validity invariants.
func ValidateBatch(b EvidenceBatch) error {
	if b.Schema != "" && b.Schema != BatchSchemaV1 {
		return fmt.Errorf("未知批次 schema %q（期望 %s）", b.Schema, BatchSchemaV1)
	}
	if b.Attempted < 0 || b.UniqueCovered < 0 || b.Positive < 0 {
		return fmt.Errorf("批次计数不能为负数")
	}
	if b.Positive > b.UniqueCovered {
		return fmt.Errorf("批次阳性数 %d 不能大于唯一覆盖数 %d", b.Positive, b.UniqueCovered)
	}
	if b.UniqueCovered > b.Attempted {
		return fmt.Errorf("唯一覆盖数不能大于尝试数")
	}
	if b.RangeStart != 0 || b.RangeEnd != 0 {
		if b.RangeStart > b.RangeEnd {
			return fmt.Errorf("批次闭区间非法: %d > %d", b.RangeStart, b.RangeEnd)
		}
		maxUnique := uint64(b.RangeEnd-b.RangeStart) + 1
		if uint64(b.UniqueCovered) > maxUnique {
			return fmt.Errorf("批次闭区间 %d–%d 最多包含 %d 个唯一对象，但 unique_covered=%d", b.RangeStart, b.RangeEnd, maxUnique, b.UniqueCovered)
		}
	}
	if (!b.Valid || !b.PreconditionOK) && b.UniqueCovered > 0 {
		return fmt.Errorf("无效批次（valid=%v, precondition_ok=%v）不得核销 unique_covered=%d", b.Valid, b.PreconditionOK, b.UniqueCovered)
	}
	return nil
}

// ExtractEvidenceBatches scans structured JSON lines or objects in tool output
// for vestrail.batch.v1 payloads.
func ExtractEvidenceBatches(text string) []EvidenceBatch {
	if !strings.Contains(text, BatchSchemaV1) {
		return nil
	}
	var out []EvidenceBatch
	tryDecode := func(raw string) {
		raw = strings.TrimSpace(raw)
		if !strings.HasPrefix(raw, "{") {
			return
		}
		var b EvidenceBatch
		if json.Unmarshal([]byte(raw), &b) == nil && b.Schema == BatchSchemaV1 {
			out = append(out, b)
		}
	}
	tryDecode(text)
	if len(out) > 0 {
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, BatchSchemaV1) {
			continue
		}
		if idx := strings.IndexByte(line, '{'); idx >= 0 {
			tryDecode(line[idx:])
		}
	}
	return out
}

const factRangeNumber = `(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)`

var factRangeCount = regexp.MustCompile(
	`(?:^|[\n。!?！？;；])[ \t　]*(?:[-*][ \t]+)?` +
		`(?:(?:闭区间|区间|端口段|端口范围|扫描范围|全范围|范围)[ \t　]*[:：]?[ \t　]*)?` +
		`(` + factRangeNumber + `)[ \t　]*[-–][ \t　]*(` + factRangeNumber +
		`)[ \t　]*(?:[,，][ \t　]*)?共[ \t　]*(` + factRangeNumber + `)[ \t　]*(?:个[ \t　]*)?(?:端口|项)`,
)

// ValidateFactRangeConsistency checks explicit inclusive-range totals in fact text.
// A qualifier after the unit can describe a subset, so it is left uninterpreted.
func ValidateFactRangeConsistency(summary, detail string) error {
	for _, content := range []string{summary, detail} {
		for _, match := range factRangeCount.FindAllStringSubmatchIndex(content, -1) {
			suffix := strings.TrimLeft(content[match[1]:], " \t　")
			next, _ := utf8.DecodeRuneInString(suffix)
			if suffix != "" && !strings.ContainsRune("，,。.;；!！?？:：)）]\n\r", next) {
				continue
			}
			startText, endText := content[match[2]:match[3]], content[match[4]:match[5]]
			countText := content[match[6]:match[7]]
			start, startErr := strconv.ParseInt(strings.ReplaceAll(startText, ",", ""), 10, 64)
			end, endErr := strconv.ParseInt(strings.ReplaceAll(endText, ",", ""), 10, 64)
			count, countErr := strconv.ParseUint(strings.ReplaceAll(countText, ",", ""), 10, 64)
			if startErr != nil || endErr != nil || countErr != nil || start > end {
				continue
			}
			expected := uint64(end-start) + 1
			if count != expected {
				return fmt.Errorf("闭区间 %s–%s 实际包含 %d 个唯一对象，请区分请求数与唯一覆盖数后重写", startText, endText, expected)
			}
		}
	}
	return nil
}

// ValidateFactBatches validates every structured batch attached to a fact.
func ValidateFactBatches(batches []EvidenceBatch) error {
	for i, b := range batches {
		if err := ValidateBatch(b); err != nil {
			return fmt.Errorf("batches[%d]: %w", i, err)
		}
	}
	return nil
}

var exclusiveRootCauseRE = regexp.MustCompile(`(?i)(不是代理|非代理问题|排除代理|not a proxy|definitely not proxy|ruled out proxy)`)
var controlOnlyHintRE = regexp.MustCompile(`(?i)(控制端口正常|control port.*ok|health.*ok|连通正常)`)

// NormalizeFactEpistemics downgrades overconfident exclusive root-cause claims
// backed only by a local health/control observation (F28), returning the effective
// confidence and any advisory note.
func NormalizeFactEpistemics(summary, detail, confidence string, unverifiedAssumptions []string) (string, string) {
	conf := strings.ToLower(strings.TrimSpace(confidence))
	if conf == "" {
		return "", ""
	}
	if conf != "observed" {
		return conf, ""
	}
	if len(unverifiedAssumptions) > 0 {
		return "inferred", "存在尚未排除的假设，confidence 已降级为 inferred"
	}
	combined := summary + "\n" + detail
	if exclusiveRootCauseRE.MatchString(combined) && controlOnlyHintRE.MatchString(combined) {
		return "inferred", "局部控制/连通观测不足以单独排除选择性通道问题，confidence 已降级为 inferred；如需确认请补充对照实验"
	}
	return conf, ""
}

// ProjectFactCompact is the single source of truth for compact fact projections
// across list_facts, graph_overview, and planner_epoch deltas (F11).
func ProjectFactCompact(id int64, state string, payload map[string]any, summaryCap int) map[string]any {
	m := map[string]any{
		"id":    id,
		"state": state,
	}
	if payload == nil {
		m["confidence"] = "unspecified"
		return m
	}
	summary, _ := payload["summary"].(string)
	if summaryCap > 0 {
		runes := []rune(summary)
		if len(runes) > summaryCap {
			m["summary"] = string(runes[:summaryCap]) + "…"
			m["summary_truncated"] = true
		} else {
			m["summary"] = summary
		}
	} else {
		m["summary"] = summary
	}
	conf, _ := payload["confidence"].(string)
	conf = strings.TrimSpace(conf)
	if conf == "" {
		conf = "unspecified"
	}
	m["confidence"] = conf
	if prov, ok := payload["provenance"].(string); ok && strings.TrimSpace(prov) != "" {
		m["provenance"] = strings.TrimSpace(prov)
	}
	if ct, ok := payload["claim_type"].(string); ok && strings.TrimSpace(ct) != "" {
		m["claim_type"] = strings.TrimSpace(ct)
	}
	if re, ok := payload["reentry_when"].(string); ok && strings.TrimSpace(re) != "" {
		m["reentry_when"] = truncRunes(strings.TrimSpace(re), 120)
	}
	if cp, ok := payload["checkpoint"]; ok && cp != nil {
		m["has_checkpoint"] = true
	}
	if ua, ok := payload["unverified_assumptions"].([]any); ok && len(ua) > 0 {
		m["has_unverified_assumptions"] = true
	}
	return m
}

func truncRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
