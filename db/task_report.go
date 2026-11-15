package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// A report is deliberately bounded. The limits describe the export, never the
// assessment's success. All reads share one short repeatable-read snapshot.
const (
	ReportNodeLimit         = 10000
	ReportPayloadLimit      = 128 << 10
	ReportTotalPayloadLimit = 16 << 20
)

type TaskReportSnapshot struct {
	TaskID        int64          `json:"task_id"`
	Title         string         `json:"title"`
	Goal          string         `json:"goal"`
	Status        string         `json:"status"`
	AsOf          time.Time      `json:"as_of"`
	ScopeRows     int            `json:"scope_rows"`
	AssetCounts   map[string]int `json:"asset_counts"`
	Findings      []*Node        `json:"findings"`
	Goals         []*Node        `json:"goals"`
	FindingsTotal int            `json:"findings_total"`
	GoalsTotal    int            `json:"goals_total"`
	Limitations   []string       `json:"limitations"`
}

func (d *DB) TaskReport(ctx context.Context, taskID int64) (*TaskReportSnapshot, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("invalid task id")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snapshot := &TaskReportSnapshot{TaskID: taskID, AssetCounts: map[string]int{}, Findings: []*Node{}, Goals: []*Node{}, Limitations: []string{}}
	var expID int64
	err = tx.QueryRowContext(ctx, `SELECT exploration_id,COALESCE(NULLIF(name,''),description),goal,status,transaction_timestamp()
FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL`, taskID).
		Scan(&expID, &snapshot.Title, &snapshot.Goal, &snapshot.Status, &snapshot.AsOf)
	if err != nil {
		return nil, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_scope WHERE task_id=$1`, taskID).Scan(&snapshot.ScopeRows); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH `+covTargetCTE+` SELECT type,COUNT(*) FROM target GROUP BY type ORDER BY type`, taskID, expID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var typ string
		var count int
		if err = rows.Scan(&typ, &count); err != nil {
			rows.Close()
			return nil, err
		}
		snapshot.AssetCounts[typ] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if snapshot.ScopeRows == 0 {
		snapshot.Limitations = append(snapshot.Limitations, "任务未声明资产范围；资产统计不代表全局资产或完整覆盖。")
	}
	usedBytes := 0
	for _, part := range []struct {
		kind  string
		nodes *[]*Node
		total *int
	}{
		{KindGoal, &snapshot.Goals, &snapshot.GoalsTotal}, {KindFinding, &snapshot.Findings, &snapshot.FindingsTotal},
	} {
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM exploration_nodes WHERE exploration_id=$1 AND kind=$2`, expID, part.kind).Scan(part.total); err != nil {
			return nil, err
		}
		omitted, err := reportNodes(ctx, tx, expID, part.kind, part.nodes, &usedBytes)
		if err != nil {
			return nil, err
		}
		if len(*part.nodes) < *part.total {
			snapshot.Limitations = append(snapshot.Limitations, fmt.Sprintf("%s 导出已截断：包含 %d / %d 条；达到条数或总内容上限。", part.kind, len(*part.nodes), *part.total))
		}
		if omitted > 0 {
			snapshot.Limitations = append(snapshot.Limitations, fmt.Sprintf("%s 中 %d 条内容超过单条上限，正文已明确省略；请从原始记录查看。", part.kind, omitted))
		}
	}
	if err = attachGoalAssessments(ctx, tx, expID, snapshot.Goals); err != nil {
		return nil, err
	}
	if err = populateFindingMetadata(ctx, tx, snapshot.Findings); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func reportNodes(ctx context.Context, tx *sql.Tx, expID int64, kind string, out *[]*Node, usedBytes *int) (int, error) {
	var lastID int64
	omitted := 0
	for len(*out) < ReportNodeLimit && *usedBytes < ReportTotalPayloadLimit {
		limit := min(256, ReportNodeLimit-len(*out))
		// Bound each payload in SQL, before it is decoded by the driver. Omission
		// is a host-generated column, not an agent-supplied JSON flag.
		rows, err := tx.QueryContext(ctx, `SELECT id,kind,
CASE WHEN octet_length(payload::text)>$4 THEN jsonb_build_object(
 'text',left(payload->>'text',1024),'name',left(payload->>'name',256),
 'vulnclass',left(payload->>'vulnclass',128),'severity',left(payload->>'severity',32),
 'summary','[正文超过报告单条上限，已省略；请查看原始记录]') ELSE payload END,
priority,state,COALESCE(origin,''),COALESCE(owner,''),COALESCE(blocked_reason,''),COALESCE(delete_reason,''),created_at,
octet_length(payload::text)>$4 FROM exploration_nodes
WHERE exploration_id=$1 AND id>$2 AND kind=$3 ORDER BY id LIMIT $5`, expID, lastID, kind, ReportPayloadLimit, limit)
		if err != nil {
			return omitted, err
		}
		read := 0
		full := false
		for rows.Next() {
			var n Node
			var raw []byte
			var clipped bool
			if err = rows.Scan(&n.ID, &n.Kind, &raw, &n.Priority, &n.State, &n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason, &n.CreatedAt, &clipped); err != nil {
				rows.Close()
				return omitted, err
			}
			if *usedBytes+len(raw) > ReportTotalPayloadLimit {
				full = true
				break
			}
			n.Payload = json.RawMessage(raw)
			*usedBytes += len(raw)
			*out = append(*out, &n)
			lastID = n.ID
			read++
			if clipped {
				omitted++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return omitted, err
		}
		if full || read < limit {
			break
		}
	}
	return omitted, nil
}
