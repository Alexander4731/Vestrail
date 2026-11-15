package db

import (
	"context"
	"database/sql"
	"fmt"
)

func lockTaskGraphWrite(ctx context.Context, tx *sql.Tx, expID int64) error {
	var taskID int64
	var status string
	var deleted sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT id,status,deleted_at FROM tasks WHERE exploration_id=$1 FOR UPDATE`, expID).Scan(&taskID, &status, &deleted)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if deleted.Valid {
		return ErrTaskArchiveState
	}
	if err = checkTaskArchiveWriteTx(ctx, tx, taskID); err != nil {
		return err
	}
	if IsTerminal(status) {
		return fmt.Errorf("任务已结束，不能修改目标或新建意图")
	}
	return nil
}

func (s *ExplorationStore) AllGoalsMet() (bool, error) {
	var met bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal')
AND NOT EXISTS(SELECT 1 FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal' AND state<>'met')`, s.expID).Scan(&met)
	return met, err
}

// GraphProgress tracks persisted reasoning changes, excluding activity heartbeats.
func (s *ExplorationStore) GraphProgress() (string, error) {
	var nodes, versions, edges int64
	err := s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(content_version),0),
(SELECT COUNT(*) FROM exploration_edges WHERE exploration_id=$1)
FROM exploration_nodes WHERE exploration_id=$1`, s.expID).Scan(&nodes, &versions, &edges)
	return fmt.Sprintf("%d/%d/%d", nodes, versions, edges), err
}

func (s *ExplorationStore) IntentsInState(state string) ([]*Node, error) {
	rows, err := s.db.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND kind='intent' AND state=$2 ORDER BY id`, s.expID, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

func (s *ExplorationStore) NodeCounts() (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT kind,COUNT(*) FROM exploration_nodes WHERE exploration_id=$1 GROUP BY kind`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		counts[kind] = count
	}
	return counts, rows.Err()
}
