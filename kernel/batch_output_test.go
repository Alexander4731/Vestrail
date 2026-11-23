package kernel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBatchPreviewPreservesCoverageAndDiagnostics(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("precondition verified\n")
	for i := int64(1); i <= 500; i++ {
		b := EvidenceBatch{Schema: BatchSchemaV1, BatchKey: fmt.Sprint(i), Scope: "fixture ports 1-500", Target: "example.test", Method: "tcp", RangeStart: i, RangeEnd: i, Attempted: 1, UniqueCovered: 1, Valid: true, PreconditionOK: true}
		line, _ := json.Marshal(b)
		raw.Write(line)
		raw.WriteByte('\n')
	}
	diagnostics := `{"schema":"vestrail.batch.v1","valid":false,"precondition_ok":false,"attempted":1,"unique_covered":0}` + "\n" +
		`{"schema":"vestrail.batch.v1","valid":true,"precondition_ok":true,"attempted":1,"unique_covered":1,"positive":1}` + "\n" +
		`{"schema":"vestrail.batch.v1","valid":true,"precondition_ok":true,"attempted":1,"unique_covered":1,"responses":["keep this evidence"]}` + "\nwarning: incomplete secondary target\n"
	raw.WriteString(diagnostics)
	preview, changed := CompactBatchOutput(raw.String())
	if !changed || len(preview) >= len(raw.String())/10 || !strings.Contains(preview, diagnostics) {
		t.Fatalf("preview lost diagnostics or did not compact: raw=%d preview=%d", raw.Len(), len(preview))
	}
	before, after := NewCoverageTracker("fixture"), NewCoverageTracker("fixture")
	before.Seed(ExtractEvidenceBatches(raw.String()))
	after.Seed(ExtractEvidenceBatches(preview))
	if !before.CoversRange(1, 500) || !after.CoversRange(1, 500) {
		t.Fatal("compaction changed complete coverage")
	}
	t.Logf("500 local batch records: raw=%d bytes, preview=%d bytes; coverage unchanged", raw.Len(), len(preview))
}

func TestBatchPreviewPreservesGapsAndTargetBoundaries(t *testing.T) {
	var raw strings.Builder
	for _, b := range []EvidenceBatch{
		{Target: "first.test", RangeStart: 1, RangeEnd: 10},
		{Target: "first.test", RangeStart: 21, RangeEnd: 30},
		{Target: "second.test", RangeStart: 31, RangeEnd: 40},
	} {
		b.Schema, b.Method, b.Scope = BatchSchemaV1, "tcp", "fixture"
		b.Valid, b.PreconditionOK = true, true
		b.Attempted, b.UniqueCovered = 10, 10
		line, _ := json.Marshal(b)
		raw.Write(line)
		raw.WriteByte('\n')
	}
	if preview, changed := CompactBatchOutput(raw.String()); changed || preview != raw.String() {
		t.Fatal("gaps or different targets were merged")
	}
}
