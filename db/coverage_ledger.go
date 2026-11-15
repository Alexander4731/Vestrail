package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed coverage_ledger.sql
var coverageLedgerSchema string

var ErrCoverageInput = errors.New("检查计划或结果格式无效")
var ErrCoverageConflict = errors.New("检查计划、任务范围或请求内容已变化，请刷新后重试")

type CoverageCheckSpec struct {
	TargetRef           string `json:"target_ref"`
	PrincipalRef        string `json:"principal_ref"`
	Category            string `json:"category"`
	TestID              string `json:"test_id"`
	PreconditionVersion int    `json:"precondition_version"`
	Prerequisites       string `json:"prerequisites"`
}

type CoveragePlanInput struct {
	RequestKey     string              `json:"request_key"`
	PreviousPlanID int64               `json:"previous_plan_id,string"`
	Checks         []CoverageCheckSpec `json:"checks"`
}

type CoveragePlan struct {
	ID            int64     `json:"id,string"`
	TaskID        int64     `json:"task_id,string"`
	Revision      int       `json:"revision"`
	ScopeRevision string    `json:"scope_revision"`
	Actor         string    `json:"actor"`
	CreatedAt     time.Time `json:"created_at"`
}

type CoverageAttemptInput struct {
	RequestKey     string `json:"request_key"`
	Outcome        string `json:"outcome"`
	Reason         string `json:"reason"`
	EvidenceNodeID int64  `json:"evidence_node_id,string,omitempty"`
}

type CoverageCheck struct {
	CoverageCheckSpec
	ID              int64  `json:"id,string"`
	State           string `json:"state"`
	Authority       string `json:"authority,omitempty"`
	ReportedOutcome string `json:"reported_outcome,omitempty"`
	Reason          string `json:"reason,omitempty"`
	EvidenceNodeID  *int64 `json:"evidence_node_id,string,omitempty"`
	EvidenceStale   bool   `json:"evidence_stale"`
}

type CoverageSummary struct {
	Availability string        `json:"availability"`
	Plan         *CoveragePlan `json:"plan,omitempty"`
	ScopeStale   bool          `json:"scope_stale"`
	Total        int           `json:"total"`
	Excluded     int           `json:"excluded"`
	Applicable   int           `json:"applicable"`
	Completed    int           `json:"completed"`
	Reported     int           `json:"reported"`
	Gaps         int           `json:"gaps"`
	Pct          *float64      `json:"pct"`
}

type CoverageLedger struct {
	CoverageSummary
	Items     []CoverageCheck `json:"items"`
	NextAfter *int64          `json:"next_after,string,omitempty"`
}

const coveragePlanCols = `id,task_id,revision,scope_revision,actor,created_at`

func scanCoveragePlan(row interface{ Scan(...any) error }) (*CoveragePlan, error) {
	p := &CoveragePlan{}
	err := row.Scan(&p.ID, &p.TaskID, &p.Revision, &p.ScopeRevision, &p.Actor, &p.CreatedAt)
	return p, err
}

func coverageScopeTx(ctx context.Context, tx *sql.Tx, taskID int64) (json.RawMessage, string, error) {
	var raw []byte
	var count int
	err := tx.QueryRowContext(ctx, `WITH scope AS (
SELECT id,kind,company_id,domain,net,value,asset_ids FROM task_scope WHERE task_id=$1 ORDER BY id LIMIT 2001
), snapshot AS (SELECT COALESCE(jsonb_agg(to_jsonb(scope)-'id' ORDER BY id),'[]'::jsonb) AS body,count(*) AS n FROM scope)
SELECT CASE WHEN octet_length(body::text)<=524288 THEN body END,n FROM snapshot`, taskID).Scan(&raw, &count)
	if err != nil {
		return nil, "", err
	}
	if count > 2000 || raw == nil {
		return nil, "", fmt.Errorf("%w: 任务范围超过检查计划快照上限", ErrCoverageInput)
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}

func (d *DB) CreateCoveragePlan(ctx context.Context, taskID int64, actor string, in CoveragePlanInput) (*CoveragePlan, error) {
	if taskID <= 0 || len(in.Checks) == 0 || len(in.Checks) > 1000 || in.RequestKey == "" || len(in.RequestKey) > 128 || actor == "" || len(actor) > 128 || in.PreviousPlanID < 0 {
		return nil, ErrCoverageInput
	}
	in.Checks = append([]CoverageCheckSpec(nil), in.Checks...)
	seen := map[string]bool{}
	for i := range in.Checks {
		c := &in.Checks[i]
		c.TargetRef = strings.TrimSpace(utf8Clean(c.TargetRef))
		c.PrincipalRef = strings.TrimSpace(utf8Clean(c.PrincipalRef))
		c.Category = strings.TrimSpace(utf8Clean(c.Category))
		c.TestID = strings.TrimSpace(utf8Clean(c.TestID))
		c.Prerequisites = strings.TrimSpace(utf8Clean(c.Prerequisites))
		if c.PrincipalRef == "" {
			c.PrincipalRef = "unknown"
		}
		if c.TargetRef == "" || len(c.TargetRef) > 2048 || len(c.PrincipalRef) > 128 || c.Category == "" || len(c.Category) > 128 || c.TestID == "" || len(c.TestID) > 128 || c.PreconditionVersion < 1 || len(c.Prerequisites) > 4096 {
			return nil, ErrCoverageInput
		}
		key, _ := json.Marshal([]any{c.TargetRef, c.PrincipalRef, c.Category, c.TestID, c.PreconditionVersion})
		if seen[string(key)] {
			return nil, fmt.Errorf("%w: 重复检查项", ErrCoverageInput)
		}
		seen[string(key)] = true
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrCoverageInput
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = LockTaskEvidenceTx(ctx, tx, taskID); err != nil {
		return nil, err
	}
	var previousDigest, previousActor string
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id,request_digest,actor FROM coverage_plans WHERE task_id=$1 AND request_key=$2`, taskID, in.RequestKey).Scan(&id, &previousDigest, &previousActor)
	if err == nil {
		if previousDigest != digest || previousActor != actor {
			return nil, ErrCoverageConflict
		}
		return scanCoveragePlan(tx.QueryRowContext(ctx, `SELECT `+coveragePlanCols+` FROM coverage_plans WHERE id=$1`, id))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var latest int64
	var revision int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(id),0),COALESCE(max(revision),0)+1 FROM coverage_plans WHERE task_id=$1`, taskID).Scan(&latest, &revision); err != nil {
		return nil, err
	}
	if latest != in.PreviousPlanID {
		return nil, ErrCoverageConflict
	}
	scope, scopeRevision, err := coverageScopeTx(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	p, err := scanCoveragePlan(tx.QueryRowContext(ctx, `INSERT INTO coverage_plans(task_id,revision,request_key,request_digest,scope_revision,scope_snapshot,actor)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+coveragePlanCols, taskID, revision, in.RequestKey, digest, scopeRevision, scope, actor))
	if err != nil {
		return nil, err
	}
	checks, _ := json.Marshal(in.Checks)
	_, err = tx.ExecContext(ctx, `INSERT INTO coverage_checks(plan_id,target_ref,principal_ref,category,test_id,precondition_version,prerequisites)
SELECT $1,target_ref,principal_ref,category,test_id,precondition_version,prerequisites FROM jsonb_to_recordset($2::jsonb)
AS x(target_ref text,principal_ref text,category text,test_id text,precondition_version int,prerequisites text)`, p.ID, checks)
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// RecordCoverageReport stores observations without promoting self-reported
// success to machine authority. Only a human can exclude a check from its plan.
func (d *DB) RecordCoverageReport(ctx context.Context, taskID, checkID int64, actor string, human bool, in CoverageAttemptInput) error {
	in.Reason = strings.TrimSpace(utf8Clean(in.Reason))
	if taskID <= 0 || checkID <= 0 || actor == "" || len(actor) > 128 || in.RequestKey == "" || len(in.RequestKey) > 128 || in.Reason == "" || len(in.Reason) > 8192 || in.EvidenceNodeID < 0 {
		return ErrCoverageInput
	}
	state := "needs_review"
	switch in.Outcome {
	case "reported_no_issue", "finding", "needs_review":
	case "blocked":
		state = "blocked"
	case "not_applicable":
		if !human {
			return ErrCoverageInput
		}
		state = "not_applicable"
	default:
		return ErrCoverageInput
	}
	authority := "agent_reported"
	if human {
		authority = "human_reported"
	}
	raw, _ := json.Marshal(in)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = LockTaskEvidenceTx(ctx, tx, taskID); err != nil {
		return err
	}
	var planID int64
	var scopeRevision string
	err = tx.QueryRowContext(ctx, `SELECT p.id,p.scope_revision FROM coverage_checks c JOIN coverage_plans p ON p.id=c.plan_id WHERE c.id=$1 AND p.task_id=$2`, checkID, taskID).Scan(&planID, &scopeRevision)
	if err != nil {
		return err
	}
	var previousDigest, previousAuthority, previousActor string
	err = tx.QueryRowContext(ctx, `SELECT request_digest,authority,actor FROM coverage_attempts WHERE check_id=$1 AND request_key=$2`, checkID, in.RequestKey).Scan(&previousDigest, &previousAuthority, &previousActor)
	if err == nil {
		if digest != previousDigest || authority != previousAuthority || actor != previousActor {
			return ErrCoverageConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var latest int64
	if err = tx.QueryRowContext(ctx, `SELECT max(id) FROM coverage_plans WHERE task_id=$1`, taskID).Scan(&latest); err != nil {
		return err
	}
	_, scope, err := coverageScopeTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if latest != planID || scope != scopeRevision {
		return ErrCoverageConflict
	}
	var evidence any
	var evidenceVersion any
	if in.EvidenceNodeID > 0 {
		var body []byte
		var version int64
		err = tx.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(n.payload::text)<=131072 THEN n.payload END,n.content_version
FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id WHERE t.id=$1 AND n.id=$2 AND n.kind IN ('fact','finding') FOR SHARE OF n`, taskID, in.EvidenceNodeID).Scan(&body, &version)
		if errors.Is(err, sql.ErrNoRows) || err == nil && body == nil {
			return ErrCoverageInput
		}
		if err != nil {
			return err
		}
		evidence = body
		evidenceVersion = version
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO coverage_attempts(check_id,request_key,request_digest,actor,authority,state,reported_outcome,reason,evidence_node_id,evidence_revision,evidence_snapshot)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,0),$10,$11)`, checkID, in.RequestKey, digest, actor, authority, state, in.Outcome, in.Reason, in.EvidenceNodeID, evidenceVersion, evidence)
	if err != nil {
		return err
	}
	return tx.Commit()
}

const coverageLatestSQL = ` FROM coverage_checks c
LEFT JOIN LATERAL(SELECT * FROM coverage_attempts a WHERE a.check_id=c.id ORDER BY id DESC LIMIT 1) a ON true
LEFT JOIN exploration_nodes e ON e.id=a.evidence_node_id WHERE c.plan_id=$1`
const coverageFreshSQL = `(a.evidence_node_id IS NULL OR COALESCE(e.content_version=a.evidence_revision AND e.payload=a.evidence_snapshot,false))`

func (d *DB) CoverageLedger(ctx context.Context, taskID, planID, after int64) (*CoverageLedger, error) {
	return d.coverageLedger(ctx, taskID, planID, after, true)
}

// CoverageSummary shares the ledger snapshot and aggregation without fetching
// a page of evidence and form details that an overview will discard.
func (d *DB) CoverageSummary(ctx context.Context, taskID int64) (*CoverageSummary, error) {
	out, err := d.coverageLedger(ctx, taskID, 0, 0, false)
	if err != nil {
		return nil, err
	}
	return &out.CoverageSummary, nil
}

func (d *DB) coverageLedger(ctx context.Context, taskID, planID, after int64, includeItems bool) (*CoverageLedger, error) {
	if taskID <= 0 || planID < 0 || after < 0 {
		return nil, ErrCoverageInput
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL)`, taskID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, sql.ErrNoRows
	}
	out := &CoverageLedger{CoverageSummary: CoverageSummary{Availability: "unavailable"}, Items: []CoverageCheck{}}
	out.Plan, err = scanCoveragePlan(tx.QueryRowContext(ctx, `SELECT `+coveragePlanCols+` FROM coverage_plans WHERE task_id=$1 AND ($2::bigint=0 OR id=$2) ORDER BY id DESC LIMIT 1`, taskID, planID))
	if errors.Is(err, sql.ErrNoRows) {
		out.Plan = nil
		if planID > 0 {
			return nil, sql.ErrNoRows
		}
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	out.Availability = "available"
	_, scope, err := coverageScopeTx(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	out.ScopeStale = scope != out.Plan.ScopeRevision
	err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE a.state='not_applicable' AND a.authority='human_reported' AND `+coverageFreshSQL+`),
count(*) FILTER(WHERE a.state IN ('observed_no_issue','finding') AND a.authority='machine_observed' AND a.evidence_node_id IS NOT NULL AND `+coverageFreshSQL+`),
count(*) FILTER(WHERE a.authority IN ('human_reported','agent_reported') AND a.reported_outcome IN ('reported_no_issue','finding'))`+coverageLatestSQL, out.Plan.ID).Scan(&out.Total, &out.Excluded, &out.Completed, &out.Reported)
	if err != nil {
		return nil, err
	}
	out.Applicable = out.Total - out.Excluded
	if out.ScopeStale {
		out.Completed = 0
	}
	out.Gaps = out.Applicable - out.Completed
	if out.Applicable > 0 && !out.ScopeStale {
		p := float64(out.Completed) / float64(out.Applicable)
		out.Pct = &p
	}
	if !includeItems {
		return out, tx.Commit()
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.target_ref,c.principal_ref,c.category,c.test_id,c.precondition_version,c.prerequisites,
COALESCE(a.state,'not_run'),COALESCE(a.authority,''),COALESCE(a.reported_outcome,''),COALESCE(a.reason,''),a.evidence_node_id,NOT `+coverageFreshSQL+coverageLatestSQL+` AND c.id>$2 ORDER BY c.id LIMIT 101`, out.Plan.ID, after)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c CoverageCheck
		if err = rows.Scan(&c.ID, &c.TargetRef, &c.PrincipalRef, &c.Category, &c.TestID, &c.PreconditionVersion, &c.Prerequisites, &c.State, &c.Authority, &c.ReportedOutcome, &c.Reason, &c.EvidenceNodeID, &c.EvidenceStale); err != nil {
			rows.Close()
			return nil, err
		}
		if c.EvidenceStale {
			c.State = "needs_review"
		}
		out.Items = append(out.Items, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(out.Items) > 100 {
		out.Items = out.Items[:100]
		last := out.Items[99].ID
		out.NextAfter = &last
	}
	return out, tx.Commit()
}

func (s *ExplorationStore) TaskCoverageLedger(ctx context.Context, after int64) (*CoverageLedger, error) {
	var tid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1`, s.expID).Scan(&tid); err != nil {
		return nil, err
	}
	return s.db.CoverageLedger(ctx, tid, 0, after)
}

func (s *ExplorationStore) RecordCheckReport(ctx context.Context, checkID int64, actor string, in CoverageAttemptInput) error {
	var tid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1`, s.expID).Scan(&tid); err != nil {
		return err
	}
	return s.db.RecordCoverageReport(ctx, tid, checkID, actor, false, in)
}

func (s *ExplorationStore) TaskCoverageSummary(ctx context.Context) (*CoverageSummary, error) {
	var tid int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE exploration_id=$1`, s.expID).Scan(&tid); err != nil {
		return nil, err
	}
	return s.db.CoverageSummary(ctx, tid)
}
