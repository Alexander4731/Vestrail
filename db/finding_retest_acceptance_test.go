package db

import (
	"errors"
	"testing"
)

func completedRetest(t *testing.T, d *DB, fid int64) *FindingRetest {
	t.Helper()
	r, c, _, err := d.CreateFindingRetest(t.Context(), fid, "local regression")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.StartFindingRetest(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if err = d.RecordFindingRetestResult(t.Context(), c.ID, "fixed", "model assessment", "local fixture observations"); err != nil {
		t.Fatal(err)
	}
	if err = d.FinishFindingRetest(r.ID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRetestAcceptancePreservesLaterHumanDecisions(t *testing.T) {
	for _, change := range []string{"risk_accepted", "false_positive", "change_back", "evidence", "newer_retest"} {
		t.Run(change, func(t *testing.T) {
			d, fid := retestDB(t)
			r := completedRetest(t, d, fid)
			switch change {
			case "evidence":
				if _, err := d.Exec(`UPDATE findings SET evidence='changed' WHERE id=$1`, fid); err != nil {
					t.Fatal(err)
				}
			case "newer_retest":
				completedRetest(t, d, fid)
			case "change_back":
				d.SetFindingStatus(fid, FindingRiskAccepted)
				d.SetFindingStatus(fid, FindingPending)
			default:
				d.SetFindingStatus(fid, change)
			}
			if err := d.AcceptFindingRetest(t.Context(), fid, r.ID, "human:test", "reviewed"); !errors.Is(err, ErrVerificationConflict) {
				t.Fatal("late result applied", err)
			}
			f, _ := d.GetFinding(fid)
			if f.Status == FindingFixed {
				t.Fatal("later decision overwritten")
			}
		})
	}
}

func TestRetestAcceptanceIsAuditedAndIdempotent(t *testing.T) {
	d, fid := retestDB(t)
	r := completedRetest(t, d, fid)
	if err := d.AcceptFindingRetest(t.Context(), fid, r.ID, "human:test", ""); !errors.Is(err, ErrVerificationInput) {
		t.Fatal("missing rationale", err)
	}
	if err := d.AcceptFindingRetest(t.Context(), fid, r.ID, "human:test", "compared original and current evidence"); err != nil {
		t.Fatal(err)
	}
	f, _ := d.GetFinding(fid)
	if f.Status != FindingFixed || f.StatusRevision != 1 {
		t.Fatalf("acceptance not applied %+v", f)
	}
	rows, err := d.ListFindingRetests(fid)
	if err != nil || rows[0].ApplicationState != "applied" || rows[0].AppliedBy != "human:test" || rows[0].AppliedAt == nil {
		t.Fatalf("no audit %+v %v", rows, err)
	}
	if _, err = d.SetFindingStatus(fid, FindingRiskAccepted); err != nil {
		t.Fatal(err)
	}
	if err = d.AcceptFindingRetest(t.Context(), fid, r.ID, "human:test", "compared original and current evidence"); err != nil {
		t.Fatal(err)
	}
	f, _ = d.GetFinding(fid)
	if f.Status != FindingRiskAccepted {
		t.Fatal("retry overwrote newer decision")
	}
}
