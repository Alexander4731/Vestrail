package db

import "fmt"

// Invalid legacy JSON values fall back to the intent's own directory.
const intentLaneRootSQL = `COALESCE(
CASE WHEN payload->>'effective_lane_root_id' ~ '^[0-9]{1,19}$' THEN
 CASE WHEN (payload->>'effective_lane_root_id')::numeric BETWEEN 1 AND 9223372036854775807 THEN (payload->>'effective_lane_root_id')::bigint END END,
CASE WHEN payload->>'lane_root_id' ~ '^[0-9]{1,19}$' THEN
 CASE WHEN (payload->>'lane_root_id')::numeric BETWEEN 1 AND 9223372036854775807 THEN (payload->>'lane_root_id')::bigint END END,id)`

func (s *ExplorationStore) FrontierExcludingLanes(limit int, busy []int64) ([]*Node, error) {
	if busy == nil {
		busy = []int64{}
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state='open' AND `+intentLaneRootSQL+` <> ALL($2::bigint[]) ORDER BY priority DESC,id ASC LIMIT $3`, s.expID, busy, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *ExplorationStore) SetEffectiveLane(id, root int64, depth int) error {
	result, err := s.db.Exec(`UPDATE exploration_nodes SET payload=payload||jsonb_build_object('effective_lane_root_id',$3::bigint,'effective_lane_depth',$4::integer),content_version=content_version+1 WHERE exploration_id=$1 AND id=$2 AND kind='intent'`, s.expID, id, root, depth)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("意图不存在于当前任务")
	}
	return nil
}
