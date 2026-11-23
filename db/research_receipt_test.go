package db

import (
	"fmt"
	"testing"

	"github.com/Alexander4731/Vestrail/kernel"
)

func TestReceiptAndStateCommitAtomically(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer d.Close()
	exp, err := d.CreateExploration(t.Name(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	id, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimIntent(id, "worker"); err != nil || !ok {
		t.Fatal(err)
	}
	r := kernel.RunReceipt{IntentID: id, ExplorationID: exp, SettledState: "done", DeliveryStatus: kernel.DeliveryComplete, TerminationCode: kernel.TermCompleted, ResearchGain: &kernel.ResearchGain{Evidence: 1}}
	function := fmt.Sprintf("reject_receipt_%d", id)
	if _, err := d.Exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'receipt storage unavailable'; END $$`, function)); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(fmt.Sprintf(`DROP FUNCTION %s() CASCADE`, function))
	if _, err := d.Exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON intent_run_receipts FOR EACH ROW WHEN (NEW.intent_id=%d) EXECUTE FUNCTION %s()`, function, id, function)); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleIntent(t.Context(), id, "done", "", r); err == nil {
		t.Fatal("injected write failure accepted")
	}
	node, err := s.GetNode(id)
	if err != nil || node.State != "running" {
		t.Fatalf("node=%+v err=%v", node, err)
	}
	if _, err := d.Exec(fmt.Sprintf(`DROP TRIGGER %s ON intent_run_receipts`, function)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.SettleIntent(t.Context(), id, "done", "", r); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM intent_run_receipts WHERE intent_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipts=%d err=%v", count, err)
	}
	node, _ = s.GetNode(id)
	if node.State != "done" {
		t.Fatalf("state=%s", node.State)
	}
}

func TestParentFreshnessUsesCompletedExecution(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer d.Close()
	exp, err := d.CreateExploration(t.Name(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
	s := d.Exploration(exp)
	intent, err := s.AddIntent(map[string]any{"summary": "fixture"}, 5, nil, "planner")
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.AddNodeLinked(KindFact, map[string]any{"summary": "observed before finish"}, 5, "confirmed", "worker", nil, []int64{intent}, RelYields)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimIntent(intent, "worker"); err != nil || !ok {
		t.Fatal(err)
	}
	versions, err := s.EvidenceVersions(t.Context(), []int64{old})
	if err != nil {
		t.Fatal(err)
	}
	r := kernel.RunReceipt{IntentID: intent, ExplorationID: exp, SettledState: "done", DeliveryStatus: kernel.DeliveryComplete, InputEvidenceVersions: versions, ResearchGain: &kernel.ResearchGain{Evidence: 1}}
	if err := s.SettleIntent(t.Context(), intent, "done", "", r); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.FreshParentEvidence([]int64{intent}, []int64{old})
	if err != nil || fresh[intent] {
		t.Fatalf("old evidence fresh=%v err=%v", fresh, err)
	}
	newFact, err := s.AddNodeLinked(KindFact, map[string]any{"summary": "new after finish"}, 5, "confirmed", "worker", nil, nil, RelYields)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = s.FreshParentEvidence([]int64{intent}, []int64{newFact})
	if err != nil || !fresh[intent] {
		t.Fatalf("new evidence fresh=%v err=%v", fresh, err)
	}
	if _, err := d.Exec(`UPDATE exploration_nodes SET content_version=content_version+1 WHERE id=$1`, old); err != nil {
		t.Fatal(err)
	}
	fresh, err = s.FreshParentEvidence([]int64{intent}, []int64{old})
	if err != nil || !fresh[intent] {
		t.Fatalf("updated evidence fresh=%v err=%v", fresh, err)
	}
}

func TestConfiguredAgentBudgetRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer d.Close()
	key := "test_budget_roundtrip"
	if _, err := d.Exec(`INSERT INTO agents(key,name,role,builtin,enabled) VALUES($1,'fixture','worker',false,true) ON CONFLICT(key) DO NOTHING`, key); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM agents WHERE key=$1`, key)
	defer d.Exec(`DELETE FROM settings WHERE key=$1`, "agent_budget."+key)
	zero, run := 0, 300
	policy := kernel.BudgetPolicy{SettleSeconds: 60, HardBufferSeconds: &zero}
	if err := d.SetAgentRunBudget(key, &run, &policy); err != nil {
		t.Fatal(err)
	}
	a, err := d.GetAgentByKey(key)
	if err != nil || a == nil || a.RunSecs != run || a.SettleSeconds != 60 || a.HardBufferSeconds == nil || *a.HardBufferSeconds != 0 {
		t.Fatalf("agent=%+v err=%v", a, err)
	}
	policy.HardBufferSeconds = nil
	if err := d.SetAgentRunBudget(key, nil, &policy); err != nil {
		t.Fatal(err)
	}
	a, err = d.GetAgentByKey(key)
	if err != nil || a.HardBufferSeconds != nil || a.RunSecs != run {
		t.Fatalf("default reset=%+v err=%v", a, err)
	}
}
