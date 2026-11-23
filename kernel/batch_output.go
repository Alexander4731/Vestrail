package kernel

import (
	"encoding/json"
	"io"
	"strings"
)

// MergeNegativeBatch merges complete, overlapping or adjacent negative ranges.
func MergeNegativeBatch(dst *EvidenceBatch, batch EvidenceBatch) bool {
	if dst == nil || ValidateBatch(*dst) != nil || ValidateBatch(batch) != nil ||
		!dst.Valid || !dst.PreconditionOK || !batch.Valid || !batch.PreconditionOK || dst.Positive != 0 || batch.Positive != 0 ||
		dst.UniqueCovered <= 0 || batch.UniqueCovered <= 0 || dst.UniqueCovered != dst.RangeEnd-dst.RangeStart+1 || batch.UniqueCovered != batch.RangeEnd-batch.RangeStart+1 ||
		(dst.RangeStart == 0 && dst.RangeEnd == 0) || (batch.RangeStart == 0 && batch.RangeEnd == 0) ||
		dst.Scope != batch.Scope || dst.Target != batch.Target || dst.Method != batch.Method || dst.Precondition != batch.Precondition ||
		dst.Provenance != batch.Provenance || dst.ArtifactPath != batch.ArtifactPath || dst.ArtifactDigest != batch.ArtifactDigest ||
		batch.RangeStart > dst.RangeEnd+1 || batch.RangeEnd < dst.RangeStart-1 {
		return false
	}
	dst.RangeStart, dst.RangeEnd = min(dst.RangeStart, batch.RangeStart), max(dst.RangeEnd, batch.RangeEnd)
	dst.UniqueCovered = dst.RangeEnd - dst.RangeStart + 1
	dst.Attempted += batch.Attempted
	dst.BatchKey = ""
	return true
}

// CompactBatchOutput merges adjacent, complete negative ranges with identical
// provenance. Positive, invalid, sparse and extended records remain verbatim.
func CompactBatchOutput(raw string) (string, bool) {
	if !strings.Contains(raw, BatchSchemaV1) {
		return raw, false
	}
	var output strings.Builder
	var pending *EvidenceBatch
	var original string
	count, merged := 0, false
	flush := func() {
		if pending == nil {
			return
		}
		if count > 1 {
			pending.BatchKey = ""
			body, _ := json.Marshal(pending)
			output.Write(body)
			output.WriteByte('\n')
			merged = true
		} else {
			output.WriteString(original)
		}
		pending, count = nil, 0
	}
	for line := range strings.SplitAfterSeq(raw, "\n") {
		var batch EvidenceBatch
		decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(line)))
		decoder.DisallowUnknownFields()
		var trailing any
		complete := decoder.Decode(&batch) == nil && decoder.Decode(&trailing) == io.EOF && batch.Schema == BatchSchemaV1 && ValidateBatch(batch) == nil &&
			batch.Valid && batch.PreconditionOK && batch.Positive == 0 && batch.UniqueCovered > 0 &&
			(batch.RangeStart != 0 || batch.RangeEnd != 0) && batch.UniqueCovered == batch.RangeEnd-batch.RangeStart+1
		if !complete {
			flush()
			output.WriteString(line)
			continue
		}
		if MergeNegativeBatch(pending, batch) {
			count++
		} else {
			flush()
			pending, original, count = &batch, line, 1
		}
	}
	flush()
	if !merged {
		return raw, false
	}
	return output.String(), true
}
