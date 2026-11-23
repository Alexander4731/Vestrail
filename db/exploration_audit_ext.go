package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AddAdHocIntentLinked atomically creates a single intent for MainAgent direct
// dispatch (F23/F24). When the task is actively running and not paused, it enters
// state='open' (standalone=false). When the task is paused or terminal (done /
// timeout / failed) — and not deleted or archived — it enters state='running'
// with owner='adhoc' (standalone=true) so the worker pool never sees it and the
// old queue is never disturbed.
func (s *ExplorationStore) AddAdHocIntentLinked(payload map[string]any, priority int, origin string, anchors, parents []int64, relation string) (id int64, standalone bool, err error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()
	status, paused, err := lockTaskGraphWriteMode(context.Background(), tx, s.expID, true)
	if err != nil {
		return 0, false, err
	}
	state := "open"
	owner := ""
	if paused || IsTerminal(status) {
		standalone = true
		state = "running"
		owner = "adhoc"
	}
	if err := tx.QueryRow(`
INSERT INTO exploration_nodes(exploration_id, kind, payload, priority, state, owner, origin)
VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), $7) RETURNING id`,
		s.expID, KindIntent, string(jsonbClean(raw)), priority, state, owner, origin).Scan(&id); err != nil {
		return 0, false, err
	}
	for _, a := range anchors {
		if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id, asset_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, a); err != nil {
			return 0, false, err
		}
	}
	for _, parent := range parents {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, parent, relation, id); err != nil {
			return 0, false, err
		}
	}
	return id, standalone, tx.Commit()
}

// ActivityTraceQuery configures windowed/incremental worker trace reads (F10).
type ActivityTraceQuery struct {
	NodeID       int64
	Order        string // "tail" (default) | "head"
	AfterStepID  int64  // cursor: only return rows with id > AfterStepID (ascending)
	Limit        int
	TerminalOnly bool
}

// ActivityTracePage holds one windowed trace slice plus cursor/total metadata.
type ActivityTracePage struct {
	Steps           []Activity
	TotalSteps      int
	HasOlder        bool
	HasNewer        bool
	NextAfterStepID int64
}

// ActivityTraceWindow returns a tail, head, or incremental cursor window of one
// worker's execution steps (excluding thinking/usage) (F10).
func (s *ExplorationStore) ActivityTraceWindow(q ActivityTraceQuery) (ActivityTracePage, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	var page ActivityTracePage
	var total int
	var countSQL string
	if q.TerminalOnly {
		countSQL = `
SELECT COUNT(*)
FROM activity a
JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
  AND n.state IN ('done','blocked','exhausted','stopped')
  AND a.kind NOT IN ('thinking','usage')`
	} else {
		countSQL = `
SELECT COUNT(*)
FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')`
	}
	if err := s.db.QueryRow(countSQL, s.expID, q.NodeID).Scan(&total); err != nil {
		return page, err
	}
	page.TotalSteps = total
	if total == 0 {
		return page, nil
	}

	var rows *sql.Rows
	var err error
	fetch := limit + 1
	order := strings.ToLower(strings.TrimSpace(q.Order))
	if order == "" {
		order = "tail"
	}
	if q.AfterStepID > 0 {
		if q.TerminalOnly {
			rows, err = s.db.Query(`
SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
FROM activity a
JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
  AND n.state IN ('done','blocked','exhausted','stopped')
  AND a.kind NOT IN ('thinking','usage') AND a.id > $3
ORDER BY a.id ASC LIMIT $4`, s.expID, q.NodeID, q.AfterStepID, fetch)
		} else {
			rows, err = s.db.Query(`
SELECT `+traceCols+`
FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage') AND id > $3
ORDER BY id ASC LIMIT $4`, s.expID, q.NodeID, q.AfterStepID, fetch)
		}
		if err != nil {
			return page, err
		}
		defer rows.Close()
		acts, err := scanTrace(rows)
		if err != nil {
			return page, err
		}
		if len(acts) > limit {
			page.HasNewer = true
			acts = acts[:limit]
		}
		page.HasOlder = q.AfterStepID > 0
		page.Steps = acts
		if len(acts) > 0 {
			page.NextAfterStepID = acts[len(acts)-1].ID
		} else {
			page.NextAfterStepID = q.AfterStepID
		}
		return page, nil
	}

	if order == "head" {
		if q.TerminalOnly {
			rows, err = s.db.Query(`
SELECT a.id, a.node_id, COALESCE(a.worker,''), COALESCE(a.kind,''), COALESCE(a.tool,''), a.is_error, COALESCE(a.summary,'')
FROM activity a
JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
  AND n.state IN ('done','blocked','exhausted','stopped')
  AND a.kind NOT IN ('thinking','usage')
ORDER BY a.id ASC LIMIT $3`, s.expID, q.NodeID, fetch)
		} else {
			rows, err = s.db.Query(`
SELECT `+traceCols+`
FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
ORDER BY id ASC LIMIT $3`, s.expID, q.NodeID, fetch)
		}
		if err != nil {
			return page, err
		}
		defer rows.Close()
		acts, err := scanTrace(rows)
		if err != nil {
			return page, err
		}
		if len(acts) > limit {
			page.HasNewer = true
			acts = acts[:limit]
		}
		page.Steps = acts
		if len(acts) > 0 {
			page.NextAfterStepID = acts[len(acts)-1].ID
		}
		return page, nil
	}

	// Default: "tail" — fetch the newest limit+1 rows and return them chronologically.
	if q.TerminalOnly {
		rows, err = s.db.Query(`
SELECT id, node_id, worker, kind, tool, is_error, summary FROM (
  SELECT a.id, a.node_id, COALESCE(a.worker,'') AS worker, COALESCE(a.kind,'') AS kind,
         COALESCE(a.tool,'') AS tool, a.is_error, COALESCE(a.summary,'') AS summary
  FROM activity a
  JOIN exploration_nodes n ON n.id=a.node_id AND n.exploration_id=a.exploration_id
  WHERE a.exploration_id=$1 AND a.node_id=$2 AND n.kind='intent'
    AND n.state IN ('done','blocked','exhausted','stopped')
    AND a.kind NOT IN ('thinking','usage')
  ORDER BY a.id DESC LIMIT $3
) sub ORDER BY id ASC`, s.expID, q.NodeID, fetch)
	} else {
		rows, err = s.db.Query(`
SELECT id, node_id, worker, kind, tool, is_error, summary FROM (
  SELECT `+traceCols+`
  FROM activity
  WHERE exploration_id=$1 AND node_id=$2 AND kind NOT IN ('thinking','usage')
  ORDER BY id DESC LIMIT $3
) sub ORDER BY id ASC`, s.expID, q.NodeID, fetch)
	}
	if err != nil {
		return page, err
	}
	defer rows.Close()
	acts, err := scanTrace(rows)
	if err != nil {
		return page, err
	}
	if len(acts) > limit {
		page.HasOlder = true
		acts = acts[len(acts)-limit:]
	}
	page.Steps = acts
	if len(acts) > 0 {
		page.NextAfterStepID = acts[len(acts)-1].ID
	}
	return page, nil
}

// ActivityTraceWindowWithSources queries this exploration first, then direct sources
// (restricted to terminal intents) for inherited history (F10).
func (s *ExplorationStore) ActivityTraceWindowWithSources(q ActivityTraceQuery) (ActivityTracePage, error) {
	page, err := s.ActivityTraceWindow(q)
	if err != nil || page.TotalSteps > 0 {
		return page, err
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return ActivityTracePage{}, err
	}
	for _, source := range sources {
		sq := q
		sq.TerminalOnly = true
		sp, err := source.Store.ActivityTraceWindow(sq)
		if err != nil {
			return ActivityTracePage{}, err
		}
		if sp.TotalSteps > 0 {
			for i := range sp.Steps {
				sp.Steps[i].Inherited = true
				sp.Steps[i].SourceTaskID = source.Task.TaskID
			}
			return sp, nil
		}
	}
	return ActivityTracePage{}, nil
}

// RunningIntentDigest summarizes live execution progress for one running worker (F16).
type RunningIntentDigest struct {
	Steps         int       `json:"steps"`
	RecentErrors  int       `json:"recent_errors"`
	StartedAt     time.Time `json:"-"`
	ElapsedSec    int64     `json:"elapsed_s"`
	LatestAction  string    `json:"latest_action,omitempty"`
}

// LatestRunningDigests returns compact 1-line live execution summaries for a batch
// of running intent IDs so the planner can spot scope drift early (F16).
func (s *ExplorationStore) LatestRunningDigests(intentIDs []int64) (map[int64]RunningIntentDigest, error) {
	out := make(map[int64]RunningIntentDigest, len(intentIDs))
	if len(intentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`
WITH scoped AS (
  SELECT node_id, id, kind, COALESCE(tool,'') AS tool, is_error, COALESCE(summary,'') AS summary, created_at,
         ROW_NUMBER() OVER (PARTITION BY node_id ORDER BY id DESC) AS rn,
         ROW_NUMBER() OVER (PARTITION BY node_id, (CASE WHEN kind='tool_use' THEN 1 ELSE 0 END) ORDER BY id DESC) AS tool_rn
  FROM activity
  WHERE exploration_id=$1 AND node_id = ANY($2) AND kind NOT IN ('thinking','usage')
)
SELECT node_id,
       COUNT(*) AS steps,
       COUNT(*) FILTER (WHERE rn <= 10 AND is_error) AS recent_errors,
       MIN(created_at) AS started_at,
       COALESCE(MAX(summary) FILTER (WHERE kind='tool_use' AND tool_rn=1),
                MAX(summary) FILTER (WHERE rn=1), '') AS latest_action
FROM scoped
GROUP BY node_id`, s.expID, intentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var nid int64
		var d RunningIntentDigest
		if err := rows.Scan(&nid, &d.Steps, &d.RecentErrors, &d.StartedAt, &d.LatestAction); err != nil {
			return nil, err
		}
		if !d.StartedAt.IsZero() {
			if el := int64(now.Sub(d.StartedAt).Seconds()); el > 0 {
				d.ElapsedSec = el
			}
		}
		out[nid] = d
	}
	return out, rows.Err()
}

// FactBatchFilter controls multi-query batch fact recall (F09).
type FactBatchFilter struct {
	Queries    []string
	Confidence string
	Provenance string
	Before     int64
	After      int64
	Limit      int
}

// ListFactsBatchWithSources recalls facts matching ANY keyword in filter.Queries
// (plus optional confidence/provenance/cursor filters) in a single round-trip (F09).
func (s *ExplorationStore) ListFactsBatchWithSources(filter FactBatchFilter) (nodes []*Node, matched map[int64][]string, hasMore bool, total int, err error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cleanQueries := make([]string, 0, len(filter.Queries))
	seenQ := map[string]bool{}
	for _, raw := range filter.Queries {
		q := strings.TrimSpace(raw)
		if q == "" {
			continue
		}
		low := strings.ToLower(q)
		if seenQ[low] {
			continue
		}
		seenQ[low] = true
		cleanQueries = append(cleanQueries, q)
		if len(cleanQueries) >= 12 {
			break
		}
	}
	filter.Queries = cleanQueries
	filter.Confidence = strings.TrimSpace(filter.Confidence)
	filter.Provenance = strings.TrimSpace(filter.Provenance)

	own, ownTotal, err := s.listFactsBatchSingle(filter, limit+1)
	if err != nil {
		return nil, nil, false, 0, err
	}
	merged := own
	total = ownTotal

	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, nil, false, 0, err
	}
	for _, source := range sources {
		page, cnt, err := source.Store.listFactsBatchSingle(filter, limit+1)
		if err != nil {
			return nil, nil, false, 0, err
		}
		for _, n := range page {
			merged = append(merged, markInheritedNode(n, source.Task.TaskID))
		}
		total += cnt
	}

	sort.Slice(merged, func(i, j int) bool { return merged[i].ID > merged[j].ID })
	hasMore = len(merged) > limit
	if hasMore {
		merged = merged[:limit]
	}

	matched = make(map[int64][]string, len(merged))
	if len(cleanQueries) > 0 {
		for _, n := range merged {
			text := strings.ToLower(string(n.Payload))
			var hits []string
			for _, q := range cleanQueries {
				if strings.Contains(text, strings.ToLower(q)) {
					hits = append(hits, q)
				}
			}
			if len(hits) > 0 {
				matched[n.ID] = hits
			}
		}
	}
	return merged, matched, hasMore, total, nil
}

func (s *ExplorationStore) listFactsBatchSingle(f FactBatchFilter, fetchLimit int) ([]*Node, int, error) {
	where := []string{"exploration_id=$1", "kind='fact'"}
	args := []any{s.expID}
	argIdx := 2

	if f.Before > 0 {
		where = append(where, fmt.Sprintf("id < $%d", argIdx))
		args = append(args, f.Before)
		argIdx++
	}
	if f.After > 0 {
		where = append(where, fmt.Sprintf("id > $%d", argIdx))
		args = append(args, f.After)
		argIdx++
	}
	if f.Confidence != "" {
		where = append(where, fmt.Sprintf("LOWER(COALESCE(payload->>'confidence','')) = LOWER($%d)", argIdx))
		args = append(args, f.Confidence)
		argIdx++
	}
	if f.Provenance != "" {
		where = append(where, fmt.Sprintf("LOWER(COALESCE(payload->>'provenance','')) = LOWER($%d)", argIdx))
		args = append(args, f.Provenance)
		argIdx++
	}
	if len(f.Queries) > 0 {
		where = append(where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM unnest($%d::text[]) kw
			WHERE COALESCE(payload->>'summary','') ILIKE '%%' || kw || '%%'
			   OR COALESCE(payload->>'detail','') ILIKE '%%' || kw || '%%'
		)`, argIdx))
		args = append(args, f.Queries)
		argIdx++
	}

	whereClause := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM exploration_nodes WHERE `+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}
	queryArgs := append(append([]any(nil), args...), fetchLimit)
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE `+whereClause+fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, argIdx), queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	return nodes, total, err
}

// RecentDoneIntentsForDedup fetches completed intents anchored to any of assetIDs
// plus the most recent `limit` done intents in this exploration (F15).
func (s *ExplorationStore) RecentDoneIntentsForDedup(assetIDs []int64, limit int) ([]*Node, error) {
	if limit <= 0 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if len(assetIDs) > 0 {
		rows, err = s.db.Query(`
SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='done'
  AND (
    id IN (SELECT node_id FROM exploration_anchors WHERE asset_id = ANY($2))
    OR id IN (SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='done' ORDER BY id DESC LIMIT $3)
  )
ORDER BY id DESC LIMIT $3`, s.expID, assetIDs, limit)
	} else {
		rows, err = s.db.Query(`
SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='done'
ORDER BY id DESC LIMIT $2`, s.expID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// IntentParentsMap returns src_id parents (rel='derived_from') for the given intent IDs.
func (s *ExplorationStore) IntentParentsMap(intentIDs []int64) (map[int64]map[int64]bool, error) {
	out := make(map[int64]map[int64]bool, len(intentIDs))
	if len(intentIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`
SELECT dst_id, src_id FROM exploration_edges
WHERE exploration_id=$1 AND rel=$2 AND dst_id = ANY($3)`, s.expID, RelDerivedFrom, intentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dst, src int64
		if err := rows.Scan(&dst, &src); err != nil {
			return nil, err
		}
		if out[dst] == nil {
			out[dst] = map[int64]bool{}
		}
		out[dst][src] = true
	}
	return out, rows.Err()
}

// TerminalIntentIDs returns the set of intent IDs in this exploration that have
// reached a terminal state (done/exhausted/blocked/stopped) (F22).
func (s *ExplorationStore) TerminalIntentIDs(ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(`
SELECT id FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND id = ANY($2)
  AND state IN ('done','exhausted','blocked','stopped')`, s.expID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
