package db

import (
	"context"
	"database/sql"
	"fmt"
)

func lockTaskGraphWrite(ctx context.Context, tx *sql.Tx, expID int64) error {
	_, _, err := lockTaskGraphWriteMode(ctx, tx, expID, false)
	return err
}

// lockTaskGraphWriteMode locks the owning task row and checks deletion, archive,
// and terminal status. When allowTerminalAdHoc is true, paused and terminal
// (done/timeout/failed) tasks still permit atomic creation of a single ad-hoc
// intent without reviving the old queue (F23/F24).
func lockTaskGraphWriteMode(ctx context.Context, tx *sql.Tx, expID int64, allowTerminalAdHoc bool) (status string, paused bool, err error) {
	var taskID int64
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id,status,COALESCE(paused,false),deleted_at FROM tasks WHERE exploration_id=$1 FOR UPDATE`, expID).Scan(&taskID, &status, &paused, &deleted)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if deleted.Valid {
		return status, paused, ErrTaskArchiveState
	}
	if err = checkTaskArchiveWriteTx(ctx, tx, taskID); err != nil {
		return status, paused, err
	}
	if !allowTerminalAdHoc && IsTerminal(status) {
		return status, paused, fmt.Errorf("任务已结束，不能修改目标或新建意图")
	}
	return status, paused, nil
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
