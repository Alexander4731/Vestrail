package kernel

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
)

type coverageInterval struct{ start, end int64 }

// CoverageTracker counts accepted object-set increments, independent of batch
// labels. Sparse partial ranges need explicit coverage evidence and remain
// uncredited until their actual covered slice is known.
type CoverageTracker struct {
	scope  string
	ranges map[string][]coverageInterval
	scopes map[string]int64
}

func NewCoverageTracker(scope string) *CoverageTracker {
	return &CoverageTracker{scope: scope, ranges: map[string][]coverageInterval{}, scopes: map[string]int64{}}
}

func (t *CoverageTracker) Seed(batches []EvidenceBatch) {
	for _, b := range batches {
		t.Add(b)
	}
}

// CoversRange requires continuous coverage for every observed target/method group.
func (t *CoverageTracker) CoversRange(start, end int64) bool {
	if len(t.ranges) == 0 {
		return false
	}
	for _, ranges := range t.ranges {
		covered := false
		for _, r := range ranges {
			if r.start <= start && r.end >= end {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func (t *CoverageTracker) Add(b EvidenceBatch) int64 {
	if ValidateBatch(b) != nil || !b.Valid || !b.PreconditionOK || b.UniqueCovered <= 0 {
		return 0
	}
	scope := t.scope
	if scope == "" {
		scope = strings.TrimSpace(b.Scope)
	}
	identity := strings.Join([]string{scope, strings.ToLower(strings.TrimSpace(b.Target)), strings.ToLower(strings.TrimSpace(b.Method)), strings.TrimSpace(b.Precondition)}, "\x00")
	if b.RangeStart == 0 && b.RangeEnd == 0 {
		if scope == "" {
			return 0
		}
		if _, seen := t.scopes[identity]; seen {
			return 0
		}
		t.scopes[identity] = b.UniqueCovered
		return b.UniqueCovered
	}
	if b.UniqueCovered != b.RangeEnd-b.RangeStart+1 {
		if _, known := t.ranges[identity]; !known {
			t.ranges[identity] = nil
		}
		return 0
	}
	intervals := t.ranges[identity]
	before := int64(0)
	for _, r := range intervals {
		before += r.end - r.start + 1
	}
	intervals = append(intervals, coverageInterval{b.RangeStart, b.RangeEnd})
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	merged := intervals[:0]
	for _, r := range intervals {
		if len(merged) == 0 || r.start > merged[len(merged)-1].end+1 {
			merged = append(merged, r)
		} else if r.end > merged[len(merged)-1].end {
			merged[len(merged)-1].end = r.end
		}
	}
	t.ranges[identity] = merged
	after := int64(0)
	for _, r := range merged {
		after += r.end - r.start + 1
	}
	return after - before
}

var observationVolatile = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),
	regexp.MustCompile(`\b(?:[0-9]{4}-[0-9]{2}-[0-9]{2}[T ]|)[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:?[0-9]{2})?`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`),
	regexp.MustCompile(`\b[0-9]+(?:\.[0-9]+)?(?:ms|us|µs|ns|s)\b`),
	regexp.MustCompile(`\b[0-9]{8,}\b`),
}

func NormalizeObservation(data []byte) []byte {
	for _, re := range observationVolatile {
		data = re.ReplaceAll(data, []byte("<volatile>"))
	}
	return bytes.TrimSpace(data)
}
