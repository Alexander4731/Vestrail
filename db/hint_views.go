package db

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *ExplorationStore) ActiveHints(limit int) ([]*Node, int64, int64, error) {
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()
	var resolved, active int64
	if err := tx.QueryRow(`SELECT count(*) FILTER (WHERE state='consumed'),count(*) FILTER (WHERE state<>'consumed') FROM exploration_nodes WHERE exploration_id=$1 AND kind='hint'`, s.expID).Scan(&resolved, &active); err != nil {
		return nil, 0, 0, err
	}
	rows, err := tx.Query(`SELECT `+nodeCols+` FROM exploration_nodes WHERE exploration_id=$1 AND kind='hint' AND state<>'consumed' ORDER BY id DESC LIMIT $2`, s.expID, limit)
	if err != nil {
		return nil, 0, 0, err
	}
	nodes, err := scanNodes(rows)
	rows.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, 0, err
	}
	return nodes, resolved, active, nil
}

// Resolved hints use the existing consumed state; hint_status preserves the tool-facing lifecycle.
// ResolveHints validates the complete set under a lock before changing it.
func (s *ExplorationStore) ResolveHints(ids []int64, reason string) error {
	unique := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("hint id must be positive")
		}
		unique[id] = true
	}
	ids = nil
	for id := range unique {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND kind='hint' AND id=ANY($2) FOR UPDATE`, s.expID, ids)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		count++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if count != len(ids) {
		return fmt.Errorf("hint_ids 必须全部属于本任务的 hint 节点")
	}
	if _, err := tx.Exec(`UPDATE exploration_nodes SET state='consumed',payload=payload||jsonb_build_object('hint_status','resolved','resolution_reason',$3::text),content_version=content_version+1 WHERE exploration_id=$1 AND kind='hint' AND id=ANY($2) AND state<>'consumed'`, s.expID, ids, reason); err != nil {
		return err
	}
	return tx.Commit()
}
