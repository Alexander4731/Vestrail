package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func reportTestTask(t *testing.T) (*DB, *Task) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	task, err := d.CreateTask("report fixture", "check explicit scope", nil, 0, 0)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM task_archives WHERE task_id=$1`, task.ID)
		d.DeleteTask(task.ID)
		d.Close()
	})
	return d, task
}

func TestTaskReportScopeAndMoreThanThousandFindings(t *testing.T) {
	d, task := reportTestTask(t)
	var local, foreign int64
	for i, dest := range []*int64{&local, &foreign} {
		err := d.QueryRow(`INSERT INTO assets(type,domain) VALUES('subdomain',$1) RETURNING id`, fmt.Sprintf("report-%d-%d.invalid", task.ID, i)).Scan(dest)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Exec(`DELETE FROM assets WHERE id=$1`, *dest) })
	}
	if _, err := d.Exec(`INSERT INTO task_scope(task_id,kind,domain,source) VALUES($1,'subdomain',$2,'manual')`, task.ID, fmt.Sprintf("report-%d-0.invalid", task.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := d.Exec(`INSERT INTO exploration_nodes(exploration_id,kind,payload,state)
SELECT $1,'finding',jsonb_build_object('summary','finding '||n),'confirmed' FROM generate_series(1,1001) n`, task.ExplorationID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.TaskReport(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.AssetCounts["subdomain"] != 1 || report.FindingsTotal != 1001 || len(report.Findings) != 1001 || len(report.Limitations) != 0 {
		t.Fatalf("wrong report: assets=%v total=%d included=%d limitations=%v", report.AssetCounts, report.FindingsTotal, len(report.Findings), report.Limitations)
	}
	for i := 1; i < len(report.Findings); i++ {
		if report.Findings[i].ID <= report.Findings[i-1].ID {
			t.Fatal("unstable pagination")
		}
	}
}

func TestTaskReportBoundsAndErrors(t *testing.T) {
	d, task := reportTestTask(t)
	store := d.Exploration(task.ExplorationID)
	_, err := store.AddNode(KindFinding, map[string]any{"summary": strings.Repeat("x", ReportPayloadLimit+1)}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.TaskReport(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || len(report.Findings[0].Payload) > 4096 || len(report.Limitations) < 2 {
		t.Fatalf("missing scope/payload limitations: %+v", report)
	}
	if _, err = d.TaskReport(t.Context(), 0); err == nil {
		t.Fatal("zero task accepted")
	}
	if _, err = d.TaskReport(t.Context(), 1<<60); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown task: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = d.TaskReport(ctx, task.ID); err == nil {
		t.Fatal("cancelled query became empty success")
	}
}
