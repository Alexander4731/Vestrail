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
	rows, err := s.db.Query(`SELECT i.id FROM exploration_nodes i
LEFT JOIN LATERAL (SELECT receipt_json FROM intent_run_receipts r
 WHERE r.exploration_id=i.exploration_id AND r.intent_id=i.id ORDER BY r.id DESC LIMIT 1) r ON true
WHERE i.exploration_id=$1 AND i.kind='intent' AND i.id=ANY($2)
AND EXISTS(SELECT 1 FROM exploration_nodes p WHERE p.id=ANY($3) AND p.kind IN ('fact','finding')
 AND p.state IN ('confirmed','origin') AND (
  (i.completed_at IS NOT NULL AND p.created_at>i.completed_at)
  OR ((r.receipt_json->'input_evidence_versions'->>p.id::text) IS NOT NULL
   AND p.content_version>(r.receipt_json->'input_evidence_versions'->>p.id::text)::integer)))`, s.expID, intentIDs, parentIDs)
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
	// Graph writers already use the owning task row as their short write lock.
	if _, err := tx.Exec(`SELECT id FROM tasks WHERE exploration_id=$1 FOR UPDATE`, s.expID); err != nil {
		return 0, false, false, err
	}
	if anchors == nil {
		anchors = []int64{}
	}
	if key, _ := payload["evidence_key"].(string); key != "" && payload["confidence"] == "observed" {
		var seen bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind IN ('fact','finding')
 AND state <> 'deleted' AND payload->>'evidence_key'=$2 AND COALESCE(payload->>'scope','')=$3)`, s.expID, key, payload["scope"]).Scan(&seen); err != nil {
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
	for _, parent := range parents {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, s.expID, parent, RelYields, id); err != nil {
			return 0, false, false, err
		}
	}
	return id, created, newEvidence, tx.Commit()
}

// ObservedEvidenceKey grounds an observation in an actual successful tool result.
// Input text, command comments and model prose cannot manufacture a new key.
func (s *ExplorationStore) ObservedEvidenceKey(ctx context.Context, intentID int64, evidence string) (string, error) {
	if intentID <= 0 || strings.TrimSpace(evidence) == "" {
		return "", nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(detail,'') FROM activity
WHERE exploration_id=$1 AND node_id=$2 AND kind='tool_result' AND NOT COALESCE(is_error,false)
 AND tool IN ('Bash','RunScript','WebFetch','Read','Grep') ORDER BY id DESC LIMIT 40`, s.expID, intentID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	fragments := strings.FieldsFunc(evidence, func(r rune) bool { return strings.ContainsRune("\n\r`\"→=>", r) })
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return "", err
		}
		var flat string
		if json.Unmarshal([]byte(detail), &flat) == nil {
			detail = flat
		}
		for _, line := range strings.Split(detail, "\n") {
			line = normalize(line)
			if len([]rune(line)) < 8 {
				continue
			}
			for _, fragment := range fragments {
				fragment = normalize(fragment)
				if len([]rune(fragment)) >= 8 && (strings.Contains(fragment, line) || strings.Contains(line, fragment)) {
					if len([]rune(fragment)) > 512 {
						continue
					}
					return string(kernel.NormalizeObservation([]byte(fragment))), nil
				}
			}
		}
	}
	return "", rows.Err()
}
