package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/Alexander4731/Vestrail/kernel"
)

func (s *ExplorationStore) EvidenceVersions(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,content_version FROM exploration_nodes WHERE id=ANY($1) AND kind IN ('fact','finding')`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var version int
		if err := rows.Scan(&id, &version); err != nil {
			return nil, err
		}
		out[id] = version
	}
	return out, rows.Err()
}

func (s *ExplorationStore) IntentInputEvidenceVersions(ctx context.Context, id int64) (map[int64]int, error) {
	parents, err := s.IntentParentsMap([]int64{id})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(parents[id]))
	for parent := range parents[id] {
		ids = append(ids, parent)
	}
	return s.EvidenceVersions(ctx, ids)
}

// FreshParentEvidence compares actual completion and consumed evidence versions.
func (s *ExplorationStore) FreshParentEvidence(intentIDs, parentIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(intentIDs) == 0 || len(parentIDs) == 0 {
		return out, nil
	}
	sources, err := s.DirectSourceStores()
	if err != nil {
		return nil, err
	}
	allowed := []int64{s.expID}
	for _, source := range sources {
		allowed = append(allowed, source.Store.ID())
	}
	rows, err := s.db.Query(`SELECT i.id FROM exploration_nodes i
LEFT JOIN LATERAL (SELECT receipt_json FROM intent_run_receipts r
 WHERE r.exploration_id=i.exploration_id AND r.intent_id=i.id ORDER BY r.id DESC LIMIT 1) r ON true
WHERE i.exploration_id=$1 AND i.kind='intent' AND i.id=ANY($2)
AND EXISTS(SELECT 1 FROM exploration_nodes p WHERE p.id=ANY($3) AND p.exploration_id=ANY($4) AND p.kind IN ('fact','finding')
 AND p.state IN ('confirmed','origin') AND (
  (i.completed_at IS NOT NULL AND p.created_at>i.completed_at)
  OR ((r.receipt_json->'input_evidence_versions'->>p.id::text) IS NOT NULL
   AND p.content_version>(r.receipt_json->'input_evidence_versions'->>p.id::text)::integer)
  OR ((r.receipt_json->'input_evidence_versions'->>p.id::text) IS NULL
   AND NOT EXISTS(SELECT 1 FROM exploration_edges e WHERE e.exploration_id=i.exploration_id AND e.dst_id=i.id AND e.src_id=p.id AND e.rel='derived_from')
   AND (p.created_at>i.created_at OR p.updated_at>i.completed_at))))`, s.expID, intentIDs, parentIDs, allowed)
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

// RecordFactLinked reuses an identical observation rather than manufacturing a
// new fact ID for the same evidence, conclusion, and target set.
func (s *ExplorationStore) RecordFactLinked(payload map[string]any, origin string, anchors, parents []int64) (id int64, created, newEvidence bool, err error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, false, err
	}
	defer tx.Rollback()
	var taskID int64
	var deleted sql.NullTime
	err = tx.QueryRow(`SELECT id, deleted_at FROM tasks WHERE exploration_id=$1 FOR SHARE`, s.expID).Scan(&taskID, &deleted)
	if err != nil && err != sql.ErrNoRows {
		return 0, false, false, err
	}
	if deleted.Valid {
		return 0, false, false, ErrTaskArchiveState
	}
	if taskID > 0 {
		if err := checkTaskArchiveWriteTx(context.Background(), tx, taskID); err != nil {
			return 0, false, false, err
		}
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('fact_dedup:' || $1::bigint::text))`, s.expID); err != nil {
		return 0, false, false, err
	}
	if anchors == nil {
		anchors = []int64{}
	}
	if key, _ := payload["evidence_key"].(string); key != "" && payload["confidence"] == "observed" {
		scope, _ := payload["scope"].(string)
		var seen bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind IN ('fact','finding')
 AND state <> 'deleted' AND payload->>'evidence_key'=$2 AND COALESCE(payload->>'scope','')=$3)`, s.expID, key, scope).Scan(&seen); err != nil {
			return 0, false, false, err
		}
		newEvidence = !seen
	}
	err = tx.QueryRow(`SELECT n.id FROM exploration_nodes n
WHERE n.exploration_id=$1 AND n.kind='fact' AND n.state <> 'deleted'
 AND n.payload - ARRAY['checkpoint','delivery_status','reentry_when']::text[] = $2::jsonb - ARRAY['checkpoint','delivery_status','reentry_when']::text[]
 AND NOT EXISTS(SELECT 1 FROM exploration_anchors a WHERE a.node_id=n.id AND NOT(a.asset_id=ANY($3)))
 AND NOT EXISTS(SELECT 1 FROM unnest($3::bigint[]) ids(asset_id) WHERE NOT EXISTS(SELECT 1 FROM exploration_anchors a WHERE a.node_id=n.id AND a.asset_id=ids.asset_id))
ORDER BY n.id LIMIT 1`, s.expID, string(jsonbClean(raw)), anchors).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return 0, false, false, err
	}
	created = err == sql.ErrNoRows
	if created {
		if err := tx.QueryRow(`INSERT INTO exploration_nodes(exploration_id,kind,payload,priority,state,origin)
VALUES($1,'fact',$2,5,'confirmed',$3) RETURNING id`, s.expID, string(jsonbClean(raw)), origin).Scan(&id); err != nil {
			return 0, false, false, err
		}
		for _, asset := range anchors {
			if _, err := tx.Exec(`INSERT INTO exploration_anchors(node_id,asset_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, asset); err != nil {
				return 0, false, false, err
			}
		}
	}
	if !created {
		updates := map[string]any{}
		for _, key := range []string{"checkpoint", "delivery_status", "reentry_when"} {
			if value := payload[key]; value != nil && value != "" {
				updates[key] = value
			}
		}
		if len(updates) > 0 {
			patch, err := json.Marshal(updates)
			if err != nil {
				return 0, false, false, err
			}
			if _, err := tx.Exec(`UPDATE exploration_nodes SET payload=payload || $2::jsonb, content_version=content_version+1 WHERE id=$1`, id, string(jsonbClean(patch))); err != nil {
				return 0, false, false, err
			}
		}
	}
	for _, parent := range parents {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, parent, RelYields, id); err != nil {
			return 0, false, false, err
		}
	}
	return id, created, newEvidence, tx.Commit()
}

// ObservedEvidenceKey grounds an observation in objective tool output.
// Input text, command comments and model prose cannot manufacture a new key.
func (s *ExplorationStore) ObservedEvidenceKey(ctx context.Context, intentID int64, evidence string) (string, error) {
	if intentID <= 0 || strings.TrimSpace(evidence) == "" {
		return "", nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(tool,''), COALESCE(detail,'') FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind='tool_result'
 AND tool NOT IN ('record_fact','report_finding','finish_work','add_intent','prove_goal','resolve_hint','steer_work','kill_work','TodoWrite','SearchExtraTools','record_coverage_check','add_company_scope','update_finding_report')
 ORDER BY id DESC LIMIT 120`, s.expID, intentID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	fragments := strings.FieldsFunc(evidence, func(r rune) bool { return strings.ContainsRune("\n\r`\"→=>", r) })
	for rows.Next() {
		var toolName, detail string
		if err := rows.Scan(&toolName, &detail); err != nil {
			return "", err
		}
		var flat string
		if json.Unmarshal([]byte(detail), &flat) == nil {
			detail = flat
		}
		blocked := false
		for _, prefix := range []string{"Error: invalid tool input", "已拦截", "审批拒绝", "本意图已提交明确交付回执", "当前已进入收尾结算阶段"} {
			if strings.HasPrefix(strings.TrimSpace(detail), prefix) {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		for _, rawLine := range strings.Split(detail, "\n") {
			line := normalize(rawLine)
			lower := strings.ToLower(line)
			boilerplate := strings.HasPrefix(lower, "http/")
			for _, prefix := range []string{"content-type:", "content-length:", "connection:", "date:", "server:", "cache-control:", "transfer-encoding:", "x-request-id:"} {
				boilerplate = boilerplate || strings.HasPrefix(lower, prefix)
			}
			if boilerplate || len([]rune(line)) < 8 {
				continue
			}
			for _, fragment := range fragments {
				fragment = normalize(fragment)
				if len([]rune(fragment)) >= 8 && (strings.Contains(fragment, line) || strings.Contains(line, fragment)) {
					if len([]rune(fragment)) > 512 {
						continue
					}
					return string(kernel.NormalizeObservation([]byte(toolName + "\n" + rawLine))), nil
				}
			}
		}
	}
	return "", rows.Err()
}
