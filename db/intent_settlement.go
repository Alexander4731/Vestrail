package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Alexander4731/Vestrail/kernel"
)

// SettleIntent retries safely after an uncertain commit without replaying work.
func (s *ExplorationStore) SettleIntent(ctx context.Context, id int64, state, reason string, receipts ...kernel.RunReceipt) error {
	exec, query := s.db.ExecContext, s.db.QueryRowContext
	var tx *sql.Tx
	if len(receipts) > 0 {
		var err error
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		exec, query = tx.ExecContext, tx.QueryRowContext
		var locked int64
		if err := query(ctx, `SELECT id FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 AND kind='intent' FOR UPDATE`, s.expID, id).Scan(&locked); err != nil {
			return err
		}
	}
	res, err := exec(ctx, `UPDATE exploration_nodes
SET state=$3, blocked_reason=NULLIF($4,''), content_version=content_version+1,
owner=CASE WHEN $3='paused' AND owner='adhoc' THEN owner ELSE NULL END,
completed_at=CASE WHEN $3 IN ('done','blocked','exhausted','stopped') THEN COALESCE(completed_at,now()) ELSE NULL END
WHERE exploration_id=$1 AND id=$2 AND kind='intent' AND state='running'`, s.expID, id, state, reason)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var current string
		if err := query(ctx, `SELECT state FROM exploration_nodes WHERE exploration_id=$1 AND id=$2 AND kind='intent'`, s.expID, id).Scan(&current); err != nil {
			return err
		}
		if current != state {
			return fmt.Errorf("%w: intent %d is %s", ErrIntentStateConflict, id, current)
		}
	}
	if tx != nil {
		r := receipts[0]
		if r.IntentID != id || r.ExplorationID != s.expID || r.SettledState != state {
			return fmt.Errorf("执行回执与意图状态不一致")
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		alreadySaved := false
		if changed == 0 {
			if err := query(ctx, `SELECT EXISTS(SELECT 1 FROM intent_run_receipts WHERE exploration_id=$1 AND intent_id=$2 AND receipt_json=$3::jsonb)`, s.expID, id, string(raw)).Scan(&alreadySaved); err != nil {
				return err
			}
		}
		if !alreadySaved {
			if _, err := exec(ctx, intentReceiptInsertSQL, receiptArguments(r, raw)...); err != nil {
				return err
			}
		}
		return tx.Commit()
	}
	return nil
}
