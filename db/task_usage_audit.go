package db

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// ErrUsageAuditArchived prevents missing cold-archive ledger rows from being
// presented as zero consumption. Reading this report never restores a task.
var ErrUsageAuditArchived = errors.New("task usage audit requires an online ledger")

// UsageAuditStat contains counts only. Input includes cache reads and writes;
// TotalTokens is input + output. Call IDs and attempt numbers describe only the
// attribution visible in the ledger, not every upstream retry or billed request.
type UsageAuditStat struct {
	Rows                int64      `json:"rows"`
	KnownCallIDs        int64      `json:"known_call_ids"`
	MissingCallIDs      int64      `json:"missing_call_ids"`
	RepeatedAttemptRows int64      `json:"repeated_attempt_rows"`
	ErrorRows           int64      `json:"error_rows"`
	ZeroUsageRows       int64      `json:"zero_usage_rows"`
	MissingPurpose      int64      `json:"missing_purpose"`
	MissingSession      int64      `json:"missing_session"`
	MissingWorker       int64      `json:"missing_worker"`
	InvalidCacheRows    int64      `json:"invalid_cache_rows"`
	InputTokens         int64      `json:"input_tokens"`
	OutputTokens        int64      `json:"output_tokens"`
	CacheReadTokens     int64      `json:"cache_read_tokens"`
	CacheWriteTokens    int64      `json:"cache_write_tokens"`
	UncachedInputTokens int64      `json:"uncached_input_tokens"`
	TotalTokens         int64      `json:"total_tokens"`
	NonzeroInputRows    int64      `json:"nonzero_input_rows"`
	InputP50            float64    `json:"input_p50"`
	InputP95            float64    `json:"input_p95"`
	DistinctSessions    int64      `json:"distinct_sessions"`
	FirstTurnInputP50   float64    `json:"first_turn_input_p50"`
	FirstTurnInputRows  int64      `json:"first_turn_input_rows"`
	FirstSeen           *time.Time `json:"first_seen,omitempty"`
	LastSeen            *time.Time `json:"last_seen,omitempty"`
}

type UsageAuditGroup struct {
	Key string `json:"key"`
	UsageAuditStat
}

type TaskUsageAudit struct {
	TaskID      string            `json:"task_id"`
	Source      string            `json:"source"`
	Since       *time.Time        `json:"since,omitempty"`
	GeneratedAt time.Time         `json:"generated_at"`
	Total       UsageAuditStat    `json:"total"`
	ByWorker    []UsageAuditGroup `json:"by_worker"`
	ByPurpose   []UsageAuditGroup `json:"by_purpose"`
	ByModel     []UsageAuditGroup `json:"by_model"`
}

const taskUsageAuditSQL = `
WITH ranked_usage AS (
 SELECT ts, NULLIF(btrim(call_id),'') call_id, attempt_id, status,
        NULLIF(btrim(session_id),'') session_id,
        COALESCE(NULLIF(btrim(worker),''),'(unknown)') worker,
        COALESCE(NULLIF(btrim(purpose),''),'(unknown)') purpose,
        COALESCE(NULLIF(btrim(model),''),'(unknown)') model,
        input_tokens::bigint input_tokens, output_tokens::bigint output_tokens,
        cache_read::bigint cache_read, cache_write::bigint cache_write,
        row_number() OVER (PARTITION BY NULLIF(btrim(session_id),'') ORDER BY ts,id) session_row
 FROM llm_usage
 WHERE task_id=$1
), usage AS (
 SELECT * FROM ranked_usage WHERE ($2::timestamptz IS NULL OR ts >= $2)
)
SELECT GROUPING(worker,purpose,model), COALESCE(worker,purpose,model,''),
       count(*), count(DISTINCT call_id), count(*) FILTER (WHERE call_id IS NULL),
       count(*) FILTER (WHERE attempt_id > 1), count(*) FILTER (WHERE status='error'),
       count(*) FILTER (WHERE input_tokens=0 AND output_tokens=0),
       count(*) FILTER (WHERE purpose='(unknown)'), count(*) FILTER (WHERE session_id IS NULL),
       count(*) FILTER (WHERE worker='(unknown)'),
       count(*) FILTER (WHERE cache_read < 0 OR cache_write < 0 OR cache_read+cache_write > input_tokens),
       COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
       COALESCE(sum(cache_read),0), COALESCE(sum(cache_write),0),
       COALESCE(sum(input_tokens-cache_read-cache_write),0),
       COALESCE(sum(input_tokens+output_tokens),0), count(*) FILTER (WHERE input_tokens > 0),
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY input_tokens) FILTER (WHERE input_tokens > 0),0),
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY input_tokens) FILTER (WHERE input_tokens > 0),0),
       count(DISTINCT session_id),
       COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY input_tokens)
         FILTER (WHERE session_id IS NOT NULL AND session_row=1 AND input_tokens > 0),0),
       count(*) FILTER (WHERE session_id IS NOT NULL AND session_row=1 AND input_tokens > 0),
       min(ts), max(ts)
FROM usage
GROUP BY GROUPING SETS ((), (worker), (purpose), (model))
ORDER BY GROUPING(worker,purpose,model) DESC, sum(input_tokens+output_tokens) DESC NULLS LAST,
         COALESCE(worker,purpose,model,'')`

// TaskUsageAudit reads every dimension from one database snapshot. It performs
// no model calls, task control, schema changes, archive restores, or body reads.
func (d *DB) TaskUsageAudit(ctx context.Context, taskID int64, since *time.Time) (*TaskUsageAudit, error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	var status string
	var archivedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT status, archived_at FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&status, &archivedAt); err != nil {
		return nil, err
	}
	if archivedAt.Valid || status == "archived" {
		return nil, ErrUsageAuditArchived
	}
	out := &TaskUsageAudit{
		TaskID: strconv.FormatInt(taskID, 10), Source: "llm_usage", Since: since,
		ByWorker: []UsageAuditGroup{}, ByPurpose: []UsageAuditGroup{}, ByModel: []UsageAuditGroup{},
	}
	rows, err := tx.QueryContext(ctx, taskUsageAuditSQL, out.TaskID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int
		var group UsageAuditGroup
		var first, last sql.NullTime
		u := &group.UsageAuditStat
		if err := rows.Scan(&bucket, &group.Key, &u.Rows, &u.KnownCallIDs, &u.MissingCallIDs,
			&u.RepeatedAttemptRows, &u.ErrorRows, &u.ZeroUsageRows, &u.MissingPurpose, &u.MissingSession,
			&u.MissingWorker, &u.InvalidCacheRows, &u.InputTokens, &u.OutputTokens, &u.CacheReadTokens,
			&u.CacheWriteTokens, &u.UncachedInputTokens, &u.TotalTokens, &u.NonzeroInputRows,
			&u.InputP50, &u.InputP95, &u.DistinctSessions, &u.FirstTurnInputP50, &u.FirstTurnInputRows,
			&first, &last); err != nil {
			return nil, err
		}
		if first.Valid {
			u.FirstSeen = &first.Time
		}
		if last.Valid {
			u.LastSeen = &last.Time
		}
		switch bucket {
		case 7:
			out.Total = *u
		case 3:
			out.ByWorker = append(out.ByWorker, group)
		case 5:
			out.ByPurpose = append(out.ByPurpose, group)
		case 6:
			out.ByModel = append(out.ByModel, group)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out.GeneratedAt = time.Now().UTC()
	return out, nil
}
