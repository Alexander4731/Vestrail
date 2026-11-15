package db

import (
	"encoding/json"
	"testing"
)

func TestArchiveAssetRemapInvalidatesAssessments(t *testing.T) {
	tables := map[string]json.RawMessage{
		"exploration_nodes": json.RawMessage(`[{"id":1,"content_version":4,"payload":{"asset_ids":[10]}}]`),
		"findings":          json.RawMessage(`[{"id":2,"asset_ids":[10],"evidence_version":3,"verification_evidence_version":3,"report_evidence_version":3}]`),
		"goal_assessments":  json.RawMessage(`[{"evidence_revision":4,"evidence_snapshot":{"asset_ids":[10]}}]`),
		"finding_retests":   json.RawMessage(`[{"baseline_version":3}]`),
	}
	for _, replacement := range []int64{10, 20} {
		got, err := remapArchiveAssetReferences(tables, map[int64]int64{10: replacement})
		if err != nil {
			t.Fatal(err)
		}
		for table, key := range map[string]string{"exploration_nodes": "content_version", "findings": "evidence_version"} {
			rows, err := decodeArchiveRows(got[table])
			if err != nil {
				t.Fatal(err)
			}
			version, _ := jsonInt64(rows[0][key])
			want := int64(3)
			if table == "exploration_nodes" {
				want++
			}
			if replacement != 10 {
				want++
			}
			if version != want {
				t.Fatalf("%s version=%d, want %d", table, version, want)
			}
		}
		for _, table := range []string{"goal_assessments", "finding_retests"} {
			if string(got[table]) != string(tables[table]) {
				t.Fatalf("remap rewrote immutable %s snapshot", table)
			}
		}
		rows, _ := decodeArchiveRows(got["findings"])
		for _, key := range []string{"verification_evidence_version", "report_evidence_version"} {
			if version, _ := jsonInt64(rows[0][key]); version != 3 {
				t.Fatalf("remap advanced old %s", key)
			}
		}
	}
}
