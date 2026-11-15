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

//go:embed goal_assessments.sql
var goalAssessmentSchema string

var ErrGoalAssessmentInput = errors.New("目标判定输入无效")

const (
	GoalModelAssessed = "model_assessed"
	GoalHumanAccepted = "human_accepted"
)

type GoalAssessment struct {
	ID               int64     `json:"id,string"`
	GoalID           int64     `json:"goal_id,string"`
	GoalRevision     int64     `json:"goal_revision"`
	EvidenceID       int64     `json:"evidence_id,string"`
	EvidenceRevision int64     `json:"evidence_revision"`
	EvidenceSHA256   string    `json:"evidence_sha256"`
	Method           string    `json:"method"`
	Actor            string    `json:"actor"`
	Reason           string    `json:"reason"`
	MissingReason    bool      `json:"missing_reason"`
	Stale            bool      `json:"stale"`
	CreatedAt        time.Time `json:"created_at"`
}

// Method and Actor are set by the host entry point, never decoded from an
// untrusted tool result. This API does not grant machine_checked authority.
type GoalAssessmentInput struct {
	GoalID, EvidenceID    int64
	Method, Actor, Reason string
}

// Both the write and freshness read use the same live direct-source visibility.
// Source-of-source nodes are intentionally excluded.
const goalEvidenceVisibleSQL = `(e.exploration_id=$1 OR EXISTS (
 SELECT 1 FROM tasks owner JOIN task_relations r ON r.task_id=owner.id
 JOIN tasks source ON source.id=r.source_task_id
 WHERE owner.exploration_id=$1 AND owner.deleted_at IS NULL
 AND source.exploration_id=e.exploration_id AND source.deleted_at IS NULL))`

func (s *ExplorationStore) AssessGoal(ctx context.Context, in GoalAssessmentInput) error {
	in.Reason = strings.TrimSpace(utf8Clean(in.Reason))
	in.Actor = strings.TrimSpace(utf8Clean(in.Actor))
	if in.GoalID <= 0 || in.EvidenceID <= 0 {
		return fmt.Errorf("%w: 目标和证据 ID 必须为正整数", ErrGoalAssessmentInput)
	}
	if in.Method != GoalModelAssessed && in.Method != GoalHumanAccepted {
		return fmt.Errorf("%w: 不允许的目标判定方式", ErrGoalAssessmentInput)
	}
	if len(in.Reason) > 16384 || len(in.Actor) > 128 || in.Actor == "" {
		return fmt.Errorf("%w: 判定理由过长或判定来源无效", ErrGoalAssessmentInput)
	}
	if in.Method == GoalHumanAccepted && in.Reason == "" {
		return fmt.Errorf("%w: 人工验收必须填写理由", ErrGoalAssessmentInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockTaskGraphWrite(ctx, tx, s.expID); err != nil {
		return err
	}
	var goalRaw, evidenceRaw []byte
	var goalRev, evidenceRev, evidenceExp int64
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(payload::text)<=$3 THEN payload END,content_version FROM exploration_nodes
WHERE id=$1 AND exploration_id=$2 AND kind='goal' FOR UPDATE`, in.GoalID, s.expID, ReportPayloadLimit).Scan(&goalRaw, &goalRev)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT CASE WHEN octet_length(e.payload::text)<=$3 THEN e.payload END,e.content_version,e.exploration_id
FROM exploration_nodes e WHERE e.id=$2 AND e.kind IN ('fact','finding') AND `+goalEvidenceVisibleSQL+` FOR SHARE OF e`, s.expID, in.EvidenceID, ReportPayloadLimit).Scan(&evidenceRaw, &evidenceRev, &evidenceExp)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 证据必须是本任务或直接关联任务可见的事实/漏洞", ErrGoalAssessmentInput)
	}
	if err != nil {
		return err
	}
	if goalRaw == nil || evidenceRaw == nil {
		return fmt.Errorf("%w: 判定快照过大，请引用具体的事实或发现记录", ErrGoalAssessmentInput)
	}
	// Repeated delivery of the same judgement is idempotent. A later goal edit
	// changes its revision, so it cannot silently reuse the old assessment.
	var identical bool
	err = tx.QueryRowContext(ctx, `SELECT goal_revision=$2 AND goal_snapshot=$3::jsonb
AND evidence_id=$4 AND evidence_revision=$5 AND evidence_snapshot=$6::jsonb
AND method=$7 AND actor=$8 AND reason=$9 FROM goal_assessments
WHERE goal_id=$1 ORDER BY id DESC LIMIT 1`, in.GoalID, goalRev, string(goalRaw), in.EvidenceID, evidenceRev, string(evidenceRaw), in.Method, in.Actor, utf8Clean(in.Reason)).Scan(&identical)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if identical {
		return tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, in.EvidenceID, RelProves, in.GoalID); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `UPDATE exploration_nodes SET state='met',content_version=content_version+1
WHERE id=$1 RETURNING content_version`, in.GoalID).Scan(&goalRev); err != nil {
		return err
	}
	digest := sha256.Sum256(evidenceRaw)
	_, err = tx.ExecContext(ctx, `INSERT INTO goal_assessments(exploration_id,goal_id,goal_revision,goal_snapshot,
evidence_id,evidence_exploration_id,evidence_revision,evidence_snapshot,evidence_sha256,method,actor,reason)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, s.expID, in.GoalID, goalRev, string(goalRaw), in.EvidenceID, evidenceExp, evidenceRev, string(evidenceRaw), hex.EncodeToString(digest[:]), in.Method, utf8Clean(in.Actor), utf8Clean(in.Reason))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// AttachGoalAssessments adds the latest judgement and its current applicability
// in one query. The immutable snapshots remain in the audit table and archive.
func (s *ExplorationStore) AttachGoalAssessments(ctx context.Context, nodes []*Node) error {
	return attachGoalAssessments(ctx, s.db, s.expID, nodes)
}

type nodeMetadataQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func attachGoalAssessments(ctx context.Context, q nodeMetadataQuerier, expID int64, nodes []*Node) error {
	byID := make(map[int64]*Node, len(nodes))
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == KindGoal {
			byID[n.ID] = n
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT ON(a.goal_id) a.id,a.goal_id,a.goal_revision,a.evidence_id,
a.evidence_revision,a.evidence_sha256,a.method,a.actor,a.reason,a.created_at,
NOT COALESCE(g.content_version=a.goal_revision AND g.payload=a.goal_snapshot
 AND e.content_version=a.evidence_revision AND e.payload=a.evidence_snapshot AND `+goalEvidenceVisibleSQL+`,false)
FROM goal_assessments a JOIN exploration_nodes g ON g.id=a.goal_id
LEFT JOIN exploration_nodes e ON e.id=a.evidence_id
WHERE a.exploration_id=$1 AND a.goal_id=ANY($2::bigint[]) ORDER BY a.goal_id,a.id DESC`, expID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a GoalAssessment
		if err = rows.Scan(&a.ID, &a.GoalID, &a.GoalRevision, &a.EvidenceID, &a.EvidenceRevision, &a.EvidenceSHA256, &a.Method, &a.Actor, &a.Reason, &a.CreatedAt, &a.Stale); err != nil {
			return err
		}
		a.MissingReason = a.Reason == ""
		if n := byID[a.GoalID]; n != nil {
			n.GoalAssessment = &a
		}
	}
	return rows.Err()
}

// Keep the old method for callers which do not yet supply a reason. Its
// judgement is explicitly model-assessed and marked missing_reason on read.
func (s *ExplorationStore) ProveGoal(goal, evidence int64) error {
	return s.AssessGoal(context.Background(), GoalAssessmentInput{GoalID: goal, EvidenceID: evidence, Method: GoalModelAssessed, Actor: "legacy"})
}

// GoalSnapshots returns the immutable payloads for an existing judgement. It is
// scoped by exploration so an ID alone cannot disclose another task's evidence.
func (s *ExplorationStore) GoalSnapshots(ctx context.Context, id int64) (json.RawMessage, json.RawMessage, error) {
	var goal, evidence []byte
	err := s.db.QueryRowContext(ctx, `SELECT goal_snapshot,evidence_snapshot FROM goal_assessments WHERE id=$1 AND exploration_id=$2`, id, s.expID).Scan(&goal, &evidence)
	return goal, evidence, err
}
