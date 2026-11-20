package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"testing"
	"time"
)

func usageAuditTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("VESTRAIL_USAGE_AUDIT_TEST_DSN")
	if dsn == "" {
		t.Skip("VESTRAIL_USAGE_AUDIT_TEST_DSN must point to an isolated test database")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	d := &DB{conn}
	schema := fmt.Sprintf("usage_audit_%d", time.Now().UnixNano())
	if _, err := d.Exec(`CREATE SCHEMA ` + schema); err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Exec(`SET default_transaction_read_only=off`)
		d.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		d.Close()
	})
	if _, err := d.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`
CREATE TABLE tasks(id bigint PRIMARY KEY, status text, deleted_at timestamptz, archived_at timestamptz);
CREATE TABLE llm_usage(task_id text,ts timestamptz,call_id text,attempt_id integer,
 status text,session_id text,worker text,purpose text,model text,
 input_tokens integer NOT NULL,output_tokens integer NOT NULL,
 cache_read integer NOT NULL,cache_write integer NOT NULL,id bigserial PRIMARY KEY);
INSERT INTO tasks VALUES (1,'paused',NULL,NULL),(2,'paused',NULL,NULL),(3,'paused',NULL,now()),(4,'paused',now(),NULL);`); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTaskUsageAuditPartitionsAndPercentiles(t *testing.T) {
	d := usageAuditTestDB(t)
	cutoff := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if _, err := d.Exec(`INSERT INTO llm_usage VALUES
 ('1',$1::timestamptz-interval '2 day','a',1,'ok','s1','worker','chat','m1',100,10,70,10),
 ('1',$1,'a',2,'ok','s1','worker','chat','m1',200,20,150,10),
 ('1',$1,NULL,1,'error',NULL,'planner','planning','m2',50,5,0,0),
 ('1',$1,NULL,NULL,'ok',NULL,NULL,NULL,NULL,0,0,0,0),
 ('2',$1,'foreign',1,'ok','s2','worker','chat','m1',999,99,0,0)`, cutoff); err != nil {
		t.Fatal(err)
	}
	// The method must work even when the connection forbids writes by default.
	if _, err := d.Exec(`SET default_transaction_read_only=on`); err != nil {
		t.Fatal(err)
	}
	audit, err := d.TaskUsageAudit(t.Context(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	u := audit.Total
	if u.Rows != 4 || u.InputTokens != 350 || u.OutputTokens != 35 || u.TotalTokens != 385 ||
		u.CacheReadTokens != 220 || u.CacheWriteTokens != 20 || u.UncachedInputTokens != 110 {
		t.Fatalf("incorrect or duplicated totals: %+v", u)
	}
	if u.KnownCallIDs != 1 || u.MissingCallIDs != 2 || u.RepeatedAttemptRows != 1 ||
		u.ErrorRows != 1 || u.ZeroUsageRows != 1 || u.MissingPurpose != 1 || u.MissingWorker != 1 || u.MissingSession != 2 {
		t.Fatalf("incorrect attribution: %+v", u)
	}
	if u.NonzeroInputRows != 3 || u.InputP50 != 100 || math.Abs(u.InputP95-190) > 0.001 {
		t.Fatalf("zeros distorted input percentiles: %+v", u)
	}
	if u.DistinctSessions != 1 || u.FirstTurnInputRows != 1 || u.FirstTurnInputP50 != 100 {
		t.Fatalf("missing session IDs or retries distorted the first-row sample: %+v", u)
	}
	for _, groups := range [][]UsageAuditGroup{audit.ByWorker, audit.ByPurpose, audit.ByModel} {
		var rows, input, output, total int64
		for _, g := range groups {
			rows += g.Rows
			input += g.InputTokens
			output += g.OutputTokens
			total += g.TotalTokens
		}
		if rows != u.Rows || input != u.InputTokens || output != u.OutputTokens || total != u.TotalTokens {
			t.Fatalf("dimension does not reconcile with the total: %+v", groups)
		}
	}
	if u.UncachedInputTokens+u.CacheReadTokens+u.CacheWriteTokens+u.OutputTokens != u.TotalTokens {
		t.Fatal("cache counted twice")
	}
	if u.FirstSeen == nil || !u.FirstSeen.Equal(cutoff.Add(-48*time.Hour)) || u.LastSeen == nil || !u.LastSeen.Equal(cutoff) {
		t.Fatalf("wrong time range: %+v", u)
	}
	recent, err := d.TaskUsageAudit(t.Context(), 1, &cutoff)
	if err != nil || recent.Total.Rows != 3 || recent.Total.TotalTokens != 275 || recent.Total.InputP50 != 125 {
		t.Fatalf("time filter not applied: %+v %v", recent, err)
	}
	if recent.Total.DistinctSessions != 1 || recent.Total.FirstTurnInputRows != 0 || recent.Total.FirstTurnInputP50 != 0 {
		t.Fatalf("a later call in the selected time range was relabeled as the first: %+v", recent.Total)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM tasks WHERE id=1`).Scan(&status); err != nil || status != "paused" {
		t.Fatalf("read changed paused task: %s %v", status, err)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM llm_usage`).Scan(&count); err != nil || count != 5 {
		t.Fatal("reading usage changed the ledger")
	}
}

func TestTaskUsageAuditFirstRowsUseStableIDsAndExcludeUnknownSessions(t *testing.T) {
	d := usageAuditTestDB(t)
	cutoff := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if _, err := d.Exec(`INSERT INTO llm_usage VALUES
 ('1',$1,'a1',1,'ok','s1','worker','chat','m',100,1,0,0),
 ('1',$1,'a2',1,'ok','s1','worker','chat','m',1000,1,0,0),
 ('1',$1,'b1',1,'ok','s2','worker','chat','m',300,1,0,0),
 ('1',$1,'unknown1',1,'ok',NULL,'worker','chat','m',10000,1,0,0),
 ('1',$1,'unknown2',1,'ok','  ','worker','chat','m',20000,1,0,0),
 ('1',$1,'c1',1,'error','s3','worker','chat','m',0,0,0,0),
 ('1',$1,'c2',2,'ok','s3','worker','chat','m',500,1,0,0),
 ('2',$1,'foreign',1,'ok','s1','worker','chat','m',99999,1,0,0)`, cutoff); err != nil {
		t.Fatal(err)
	}
	audit, err := d.TaskUsageAudit(t.Context(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Total.DistinctSessions != 3 || audit.Total.FirstTurnInputRows != 2 || audit.Total.FirstTurnInputP50 != 200 {
		t.Fatalf("first-row sampling included a retry, an unknown session or another task: %+v", audit.Total)
	}
	if len(audit.ByWorker) != 1 || audit.ByWorker[0].FirstTurnInputP50 != 200 {
		t.Fatalf("first-row statistics were not included in role groups: %+v", audit.ByWorker)
	}
}

func TestTaskUsageAuditEmptyArchivedDeletedAndCancelled(t *testing.T) {
	d := usageAuditTestDB(t)
	audit, err := d.TaskUsageAudit(t.Context(), 1, nil)
	if err != nil || audit.Total.Rows != 0 || audit.Total.FirstSeen != nil || len(audit.ByModel) != 0 || audit.ByWorker == nil {
		t.Fatalf("empty ledger misrepresented: %+v %v", audit, err)
	}
	if _, err := d.TaskUsageAudit(t.Context(), 3, nil); !errors.Is(err, ErrUsageAuditArchived) {
		t.Fatalf("archived usage shown as zero or task restored: %v", err)
	}
	for _, taskID := range []int64{4, 999} {
		if _, err := d.TaskUsageAudit(t.Context(), taskID, nil); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("deleted/missing task %d: %v", taskID, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.TaskUsageAudit(ctx, 1, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
}

func TestTaskUsageAuditPreservesInvalidCacheCounts(t *testing.T) {
	d := usageAuditTestDB(t)
	if _, err := d.Exec(`INSERT INTO llm_usage VALUES
 ('1',now(),'bad',1,'ok','s','worker','chat','m',5,0,7,0)`); err != nil {
		t.Fatal(err)
	}
	audit, err := d.TaskUsageAudit(t.Context(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if audit.Total.InvalidCacheRows != 1 || audit.Total.CacheReadTokens != 7 || audit.Total.UncachedInputTokens != -2 || audit.Total.TotalTokens != 5 {
		t.Fatalf("invalid provider counts were silently normalized: %+v", audit.Total)
	}
}
