package db

import (
	"context"
	"fmt"
)

// Invalid legacy JSON values fall back to the intent's own directory.
const intentLaneRootSQL = `COALESCE(
CASE WHEN payload->>'effective_lane_root_id' ~ '^[0-9]{1,19}$' THEN
 CASE WHEN (payload->>'effective_lane_root_id')::numeric BETWEEN 1 AND 9223372036854775807 THEN (payload->>'effective_lane_root_id')::bigint END END,
CASE WHEN payload->>'lane_root_id' ~ '^[0-9]{1,19}$' THEN
 CASE WHEN (payload->>'lane_root_id')::numeric BETWEEN 1 AND 9223372036854775807 THEN (payload->>'lane_root_id')::bigint END END,id)`

func (s *ExplorationStore) FrontierExcludingLanes(limit int, busy []int64) ([]*Node, error) {
	return s.FrontierExcludingLanesAfter(limit, busy, 0, 0)
}

// FrontierExcludingLanesAfter advances through priority-ordered candidate pages.
func (s *ExplorationStore) FrontierExcludingLanesAfter(limit int, busy []int64, priority int, after int64) ([]*Node, error) {
	if busy == nil {
		busy = []int64{}
	}
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes
WHERE exploration_id=$1 AND kind='intent' AND state='open' AND COALESCE(owner,'') <> 'adhoc'
  AND `+intentLaneRootSQL+` <> ALL($2::bigint[])
  AND ($5::bigint=0 OR priority<$4 OR (priority=$4 AND id>$5))
ORDER BY priority DESC,id ASC LIMIT $3`, s.expID, busy, limit, priority, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// ClaimIntentSnapshot rejects an intent edited after its admission checks.
func (s *ExplorationStore) ClaimIntentSnapshot(ctx context.Context, node *Node, expected, owner string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes SET state='running',owner=$1,completed_at=NULL,blocked_reason=NULL
WHERE id=$2 AND exploration_id=$3 AND kind='intent' AND state=$4 AND payload=$5::jsonb`, owner, node.ID, s.expID, expected, string(node.Payload))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// QueueIntentSnapshot reserves a paused intent for its human continuation.
func (s *ExplorationStore) QueueIntentSnapshot(ctx context.Context, node *Node) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exploration_nodes SET state='open',owner='adhoc',completed_at=NULL,blocked_reason=NULL
WHERE id=$1 AND exploration_id=$2 AND kind='intent' AND state='paused' AND payload=$3::jsonb`, node.ID, s.expID, string(node.Payload))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
