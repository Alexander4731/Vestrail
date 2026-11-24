package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

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

DO $$ BEGIN
 DELETE FROM intent_run_receipts r
 WHERE NOT EXISTS (SELECT 1 FROM explorations e WHERE e.id=r.exploration_id)
    OR NOT EXISTS (SELECT 1 FROM exploration_nodes n WHERE n.id=r.intent_id AND n.exploration_id=r.exploration_id AND n.kind='intent');
 DELETE FROM intent_run_receipts r USING intent_run_receipts newer
 WHERE r.exploration_id=newer.exploration_id AND r.intent_id=newer.intent_id AND r.run_seq=newer.run_seq AND r.id<newer.id;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='intent_run_receipts'::regclass AND conname='fk_receipts_exploration') THEN
  ALTER TABLE intent_run_receipts ADD CONSTRAINT fk_receipts_exploration FOREIGN KEY(exploration_id) REFERENCES explorations(id) ON DELETE CASCADE;
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='intent_run_receipts'::regclass AND conname='fk_receipts_intent') THEN
  ALTER TABLE intent_run_receipts ADD CONSTRAINT fk_receipts_intent FOREIGN KEY(intent_id) REFERENCES exploration_nodes(id) ON DELETE CASCADE;
 END IF;
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='intent_run_receipts'::regclass AND conname='uq_receipts_run') THEN
  ALTER TABLE intent_run_receipts ADD CONSTRAINT uq_receipts_run UNIQUE(exploration_id,intent_id,run_seq);
 END IF;
END $$;
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
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locked int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM exploration_nodes WHERE id=$1 AND exploration_id=$2 AND kind='intent' FOR UPDATE`, r.IntentID, r.ExplorationID).Scan(&locked); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, intentReceiptInsertSQL, receiptArguments(r, raw)...); err != nil {
		return err
	}
	return tx.Commit()
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

// IntentRunReceipt combines the durable sequence with the original execution receipt.
type IntentRunReceipt struct {
	IntentID   int64
	RunSeq     int
	HasNetGain bool
	CreatedAt  time.Time
	Receipt    kernel.RunReceipt
}

func scanIntentRunReceipt(sc interface{ Scan(...any) error }) (IntentRunReceipt, error) {
	var receipt IntentRunReceipt
	var raw []byte
	if err := sc.Scan(&receipt.IntentID, &receipt.RunSeq, &receipt.HasNetGain, &receipt.CreatedAt, &raw); err != nil {
		return receipt, err
	}
	err := json.Unmarshal(raw, &receipt.Receipt)
	return receipt, err
}

func (d *DB) IntentReceipts(ctx context.Context, explorationID, intentID int64) ([]IntentRunReceipt, error) {
	rows, err := d.QueryContext(ctx, `SELECT intent_id,run_seq,has_net_gain,created_at,receipt_json FROM intent_run_receipts WHERE exploration_id=$1 AND intent_id=$2 ORDER BY run_seq DESC`, explorationID, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IntentRunReceipt{}
	for rows.Next() {
		receipt, err := scanIntentRunReceipt(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, receipt)
	}
	return out, rows.Err()
}

func (d *DB) LatestIntentReceipts(ctx context.Context, ids []int64) (map[int64]IntentRunReceipt, error) {
	out := map[int64]IntentRunReceipt{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT ON(intent_id) intent_id,run_seq,has_net_gain,created_at,receipt_json FROM intent_run_receipts WHERE intent_id=ANY($1) ORDER BY intent_id,run_seq DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		receipt, err := scanIntentRunReceipt(rows)
		if err != nil {
			return nil, err
		}
		out[receipt.IntentID] = receipt
	}
	return out, rows.Err()
}
