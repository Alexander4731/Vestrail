package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
)

func TestAuditFix_FactRangeConsistencyOnWrite(t *testing.T) {
	pg := testDB(t)
	defer pg.Close()
	id, err := pg.CreateExploration("range fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Exec(`DELETE FROM explorations WHERE id=$1`, id)
	store := pg.Exploration(id)
	tools := NewToolSet(store, "test")
	recordFact := tools.recordFact()

	result, err := recordFact.Call(t.Context(), json.RawMessage(`{"summary":"22001-23000 共 2000 端口","confidence":"observed"}`), nil)
	if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "1000 个唯一对象") {
		t.Fatalf("expected range validation error, got %q (%v)", result.Flatten(), err)
	}
	if tools.Writes().Facts != 0 {
		t.Fatalf("invalid claim was counted as a fact: %+v", tools.Writes())
	}

	result, err = recordFact.Call(t.Context(), json.RawMessage(`{"facts":[
		{"summary":"invalid fixture", "detail":"22,001–23,000，共 2,000 个端口"},
		{"summary":"22,001–23,000，共 1,000 个端口","confidence":"observed"},
		{"summary":"22001–23000，共 12 个端口开放"}
	]}`), nil)
	if err != nil || result.IsError {
		t.Fatalf("batch call failed: %q (%v)", result.Flatten(), err)
	}
	var batch struct {
		IDs    []int64           `json:"ids"`
		Errors map[string]string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(result.Flatten()), &batch); err != nil {
		t.Fatal(err)
	}
	if len(batch.IDs) != 3 || batch.IDs[0] != 0 || batch.IDs[1] <= 0 || batch.IDs[2] <= 0 ||
		len(batch.Errors) != 1 || !strings.Contains(batch.Errors["0"], "1000 个唯一对象") {
		t.Fatalf("wrong per-item result: %+v", batch)
	}
	nodes, err := store.ListByKind(db.KindFact, 100)
	if err != nil || len(nodes) != 2 || tools.Writes().Facts != 2 {
		t.Fatalf("wanted exactly two valid writes, got %d nodes and %+v (%v)", len(nodes), tools.Writes(), err)
	}
}
