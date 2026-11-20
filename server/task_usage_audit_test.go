package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Alexander4731/Vestrail/db"
)

func TestTaskUsageAuditValidatesRequestBeforeDatabase(t *testing.T) {
	s := &Server{m: &Manager{}}
	for _, tc := range []struct {
		id, hours string
		want      int
	}{
		{"invalid", "0", 400}, {"0", "0", 400}, {"-1", "0", 400},
		{"1", "-1", 400}, {"1", "8761", 400}, {"1", "bad", 400},
		{"1", "0", 503}, {"1", "24", 503}, {"1", "8760", 503},
	} {
		r := httptest.NewRequest(http.MethodGet, "/?hours="+tc.hours, nil)
		r.SetPathValue("id", tc.id)
		w := httptest.NewRecorder()
		s.taskUsageAudit(w, r)
		if w.Code != tc.want {
			t.Fatalf("id=%s hours=%s: got %d, want %d: %s", tc.id, tc.hours, w.Code, tc.want, w.Body.String())
		}
	}
}

func TestTaskUsageAuditHTTPUsesLedgerAndRejectsArchives(t *testing.T) {
	dsn := os.Getenv("VESTRAIL_USAGE_AUDIT_TEST_DSN")
	if dsn == "" {
		t.Skip("VESTRAIL_USAGE_AUDIT_TEST_DSN must point to an isolated test database")
	}
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	pg := &db.DB{DB: conn}
	schema := fmt.Sprintf("usage_http_%d", time.Now().UnixNano())
	if _, err := pg.Exec(`CREATE SCHEMA ` + schema); err != nil {
		pg.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pg.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		pg.Close()
	})
	if _, err := pg.Exec(`SET search_path TO ` + schema); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.Exec(`
CREATE TABLE tasks(id bigint PRIMARY KEY,status text,deleted_at timestamptz,archived_at timestamptz);
CREATE TABLE llm_usage(task_id text,ts timestamptz,call_id text,attempt_id integer,
 status text,session_id text,worker text,purpose text,model text,
 input_tokens integer,output_tokens integer,cache_read integer,cache_write integer,id bigserial PRIMARY KEY);
INSERT INTO tasks VALUES (1,'paused',NULL,NULL),(2,'paused',NULL,now());
INSERT INTO llm_usage VALUES ('1',now(),'call',1,'ok','s1','worker','chat','fixture',100000,10000,80000,5000);`); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/tasks/{id}/usage-audit", s.taskUsageAudit)
	for _, tc := range []struct {
		id   string
		want int
	}{{"1", 200}, {"2", 409}, {"999", 404}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tasks/"+tc.id+"/usage-audit?hours=24", nil))
		if w.Code != tc.want {
			t.Fatalf("task=%s: status %d: %s", tc.id, w.Code, w.Body.String())
		}
		if w.Code == http.StatusOK {
			var result db.TaskUsageAudit
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total.TotalTokens != 110000 || result.Total.UncachedInputTokens != 15000 || result.Since == nil || len(result.ByWorker) != 1 {
				t.Fatalf("HTTP report lost ledger values: %+v", result)
			}
			if result.Total.DistinctSessions != 1 || result.Total.FirstTurnInputP50 != 100000 || result.Total.FirstTurnInputRows != 1 {
				t.Fatalf("HTTP report lost session statistics: %+v", result.Total)
			}
		}
	}
	var status string
	if err := pg.QueryRow(`SELECT status FROM tasks WHERE id=2`).Scan(&status); err != nil || status != "paused" {
		t.Fatalf("reading an archive changed the task: %s %v", status, err)
	}
}
