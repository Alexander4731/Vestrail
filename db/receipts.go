package db

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Alexander4731/Vestrail/kernel"
)

const intentReceiptsSchema = `
CREATE TABLE IF NOT EXISTS intent_run_receipts (
    id               BIGSERIAL PRIMARY KEY,
    task_id          BIGINT NOT NULL,
    exploration_id   BIGINT NOT NULL,
    intent_id        BIGINT NOT NULL,
    run_seq          INTEGER NOT NULL DEFAULT 1,
    termination_code TEXT NOT NULL,
    delivery_status  TEXT NOT NULL,
    settled_state    TEXT NOT NULL,
    has_net_gain     BOOLEAN NOT NULL DEFAULT FALSE,
    receipt_json     JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_intent_run_receipts_lookup
    ON intent_run_receipts (exploration_id, intent_id, id DESC);
`

const intentReceiptInsertSQL = `
INSERT INTO intent_run_receipts(
    task_id, exploration_id, intent_id, run_seq,
    termination_code, delivery_status, settled_state, has_net_gain, receipt_json
)
VALUES (
    $1, $2, $3,
    COALESCE((SELECT MAX(run_seq)+1 FROM intent_run_receipts WHERE exploration_id=$2 AND intent_id=$3), 1),
    $4, $5, $6, $7, $8::jsonb
)`

func receiptArguments(r kernel.RunReceipt, raw []byte) []any {
	return []any{r.TaskID, r.ExplorationID, r.IntentID,
		string(r.TerminationCode), string(r.DeliveryStatus), r.SettledState, r.HasNetGain(), string(jsonbClean(raw))}
}

// SaveIntentReceipt persists a receipt independently of a state transition.
func (d *DB) SaveIntentReceipt(ctx context.Context, r kernel.RunReceipt) error {
	if d == nil || r.IntentID <= 0 || r.ExplorationID <= 0 {
		return nil
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, intentReceiptInsertSQL, receiptArguments(r, raw)...)
	return err
}

// LatestIntentReceipt returns the most recent RunReceipt persisted for (explorationID, intentID).
func (d *DB) LatestIntentReceipt(ctx context.Context, explorationID, intentID int64) (*kernel.RunReceipt, error) {
	if d == nil || explorationID <= 0 || intentID <= 0 {
		return nil, nil
	}
	var raw []byte
	err := d.QueryRowContext(ctx, `
SELECT receipt_json
FROM intent_run_receipts
WHERE exploration_id=$1 AND intent_id=$2
ORDER BY id DESC LIMIT 1`, explorationID, intentID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r kernel.RunReceipt
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// LatestIntentReceipt on ExplorationStore delegates to the underlying DB.
func (s *ExplorationStore) LatestIntentReceipt(ctx context.Context, intentID int64) (*kernel.RunReceipt, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return s.db.LatestIntentReceipt(ctx, s.expID, intentID)
}
