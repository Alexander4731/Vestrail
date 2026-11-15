package db

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func coveragePlanFixture(t *testing.T, d *DB, task *Task, number int) *CoveragePlan {
	t.Helper()
	checks := make([]CoverageCheckSpec, number)
	for i := range checks {
		checks[i] = CoverageCheckSpec{TargetRef: "https://fixture.invalid/resource", PrincipalRef: "anonymous", Category: "access-control", TestID: fmt.Sprintf("read-%d", i), PreconditionVersion: 1, Prerequisites: "authorized local fixture"}
	}
	p, err := d.CreateCoveragePlan(t.Context(), task.ID, "human:test", CoveragePlanInput{RequestKey: "plan", Checks: checks})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCoverageLedgerCountsReportsAsGaps(t *testing.T) {
	d, task := reportTestTask(t)
	unavailable, err := d.CoverageLedger(t.Context(), task.ID, 0, 0)
	if err != nil || unavailable.Availability != "unavailable" || unavailable.Pct != nil {
		t.Fatalf("no plan %+v %v", unavailable, err)
	}
	p := coveragePlanFixture(t, d, task, 2)
	ledger, err := d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Total != 2 || ledger.Gaps != 2 || ledger.Completed != 0 {
		t.Fatal("unrun checks counted complete", ledger)
	}
	evidence, err := d.Exploration(task.ExplorationID).AddNode(KindFact, map[string]any{"summary": "homepage fingerprint"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	in := CoverageAttemptInput{RequestKey: "observation", Outcome: "reported_no_issue", Reason: "only a homepage fingerprint", EvidenceNodeID: evidence}
	for range 2 {
		if err = d.RecordCoverageReport(t.Context(), task.ID, ledger.Items[0].ID, "agent:test", false, in); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err = d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Total != 2 || ledger.Reported != 1 || ledger.Completed != 0 || ledger.Gaps != 2 || ledger.Items[0].State != "needs_review" {
		t.Fatalf("self report completed test: %+v", ledger)
	}
	if _, err = d.Exec(`UPDATE exploration_nodes SET payload='{}' WHERE id=$1`, evidence); err != nil {
		t.Fatal(err)
	}
	ledger, err = d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil || !ledger.Items[0].EvidenceStale {
		t.Fatal("changed evidence not visible", err)
	}
	if err = d.RecordCoverageReport(t.Context(), task.ID, ledger.Items[1].ID, "agent:test", false, CoverageAttemptInput{RequestKey: "exclude", Outcome: "not_applicable", Reason: "model wants exclusion"}); !errors.Is(err, ErrCoverageInput) {
		t.Fatal("model excluded check", err)
	}
	if err = d.RecordCoverageReport(t.Context(), task.ID, ledger.Items[1].ID, "human:test", true, CoverageAttemptInput{RequestKey: "exclude", Outcome: "not_applicable", Reason: "endpoint not part of this workflow"}); err != nil {
		t.Fatal(err)
	}
	ledger, err = d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil || ledger.Excluded != 1 || ledger.Applicable != 1 || ledger.Gaps != 1 {
		t.Fatalf("wrong denominator %+v %v", ledger, err)
	}
}

func TestCoverageLedgerScopeRevisionAndOwnership(t *testing.T) {
	d, task := reportTestTask(t)
	p := coveragePlanFixture(t, d, task, 1)
	ledger, err := d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.CreateTask("other coverage", "fixture", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	if err = d.RecordCoverageReport(t.Context(), other.ID, ledger.Items[0].ID, "human:test", true, CoverageAttemptInput{RequestKey: "bad", Outcome: "blocked", Reason: "other task"}); err == nil {
		t.Fatal("cross task check write")
	}
	if _, err = d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES($1,'subdomain','new.fixture.invalid','manual')`, task.ID); err != nil {
		t.Fatal(err)
	}
	ledger, err = d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil || !ledger.ScopeStale || ledger.Pct != nil {
		t.Fatalf("scope stale %+v %v", ledger, err)
	}
	if err = d.RecordCoverageReport(t.Context(), task.ID, ledger.Items[0].ID, "human:test", true, CoverageAttemptInput{RequestKey: "stale", Outcome: "blocked", Reason: "old scope"}); !errors.Is(err, ErrCoverageConflict) {
		t.Fatal("old scope accepted", err)
	}
	input := CoveragePlanInput{RequestKey: "revision", PreviousPlanID: p.ID, Checks: []CoverageCheckSpec{ledger.Items[0].CoverageCheckSpec}}
	var wg sync.WaitGroup
	ids := make(chan int64, 6)
	for range 6 {
		wg.Go(func() {
			next, err := d.CreateCoveragePlan(t.Context(), task.ID, "human:test", input)
			if err != nil {
				t.Error(err)
				return
			}
			ids <- next.ID
		})
	}
	wg.Wait()
	close(ids)
	var nextID int64
	for id := range ids {
		if nextID != 0 && id != nextID {
			t.Fatal("duplicate plan retries")
		}
		nextID = id
	}
	next, err := d.CoverageLedger(t.Context(), task.ID, 0, 0)
	if err != nil || next.Plan.ID != nextID || next.Plan.Revision != 2 || next.ScopeStale || next.Total != 1 {
		t.Fatalf("new revision %+v %v", next, err)
	}
	if _, err = d.CreateCoveragePlan(t.Context(), task.ID, "human:other", input); !errors.Is(err, ErrCoverageConflict) {
		t.Fatal("plan request key reused by another actor", err)
	}
	summary, err := d.Exploration(task.ExplorationID).TaskCoverageSummary(t.Context())
	if err != nil || !reflect.DeepEqual(*summary, next.CoverageSummary) {
		t.Fatalf("summary differs from ledger: %+v %v", summary, err)
	}
}

func TestCoverageLedgerPaginationAndZeroDenominator(t *testing.T) {
	d, task := reportTestTask(t)
	p := coveragePlanFixture(t, d, task, 101)
	first, err := d.CoverageLedger(t.Context(), task.ID, p.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 100 || first.NextAfter == nil || first.Total != 101 {
		t.Fatal("lost coverage checks", first)
	}
	last, err := d.CoverageLedger(t.Context(), task.ID, p.ID, *first.NextAfter)
	if err != nil || len(last.Items) != 1 || last.NextAfter != nil {
		t.Fatalf("bad last page %+v %v", last, err)
	}
	next, err := d.CreateCoveragePlan(t.Context(), task.ID, "human:test", CoveragePlanInput{RequestKey: "one", PreviousPlanID: p.ID, Checks: []CoverageCheckSpec{first.Items[0].CoverageCheckSpec}})
	if err != nil {
		t.Fatal(err)
	}
	one, err := d.CoverageLedger(t.Context(), task.ID, next.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RecordCoverageReport(t.Context(), task.ID, one.Items[0].ID, "human:test", true, CoverageAttemptInput{RequestKey: "not-applicable", Outcome: "not_applicable", Reason: "fixture excluded"}); err != nil {
		t.Fatal(err)
	}
	one, err = d.CoverageLedger(t.Context(), task.ID, next.ID, 0)
	if err != nil || one.Applicable != 0 || one.Pct != nil {
		t.Fatal("zero denominator looks 100%", one, err)
	}
}
