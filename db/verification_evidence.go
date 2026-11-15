package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
)

// VerificationReceipt refers to executor-owned rows only. Legacy approval
// summaries are intentionally excluded. The graph edge binds a receipt to the
// finding's originating intent; a successful task alone is insufficient.
type VerificationReceipt struct {
	ID           int64  `json:"id,string"`
	Version      int64  `json:"version"`
	TaskID       string `json:"task_id"`
	IntentID     int64  `json:"intent_id,string"`
	RunID        string `json:"run_id"`
	ToolUseID    string `json:"tool_use_id"`
	InputDigest  string `json:"input_digest"`
	State        string `json:"state"`
	OutputSHA256 string `json:"output_sha256"`
	Complete     bool   `json:"complete"`
}

const verificationReceiptIdentitySQL = `f.task_id::text=er.task_id
AND er.run_id<>'' AND er.tool_use_id<>'' AND er.input_digest<>''
AND ip.task_id=er.task_id AND ip.audit->>'run_id'=er.run_id
AND ip.audit->>'tool_use_id'=er.tool_use_id AND ip.audit->>'input_digest'=er.input_digest
AND ip.audit->>'effective_action'='allow'
AND EXISTS(SELECT 1 FROM exploration_edges e WHERE e.src_id=er.intent_id AND e.dst_id=f.node_id AND e.rel='yields')`

// Receipt versions can change after a check has completed (late executor output
// or recovery). Compute freshness on read; updating findings from a receipt
// writer would invert the finding -> receipt lock order used by verification.
const verificationStaleSQL = `(f.verification_attempt_id IS NOT NULL AND (
f.verification_evidence_version<>f.evidence_version OR NOT EXISTS (
 SELECT 1 FROM finding_verification_attempts va WHERE va.id=f.verification_attempt_id AND va.finding_id=f.id
 AND (va.verifier<>'execution_receipt' OR va.assurance='none' OR EXISTS (
  SELECT 1 FROM tool_execution_receipts er JOIN intercept_pending ip ON ip.id=er.approval_id
  WHERE er.approval_id=(va.evidence_refs->0->>'id')::bigint AND er.version=(va.evidence_refs->0->>'version')::bigint
  AND ` + verificationReceiptIdentitySQL + `)))))`

func VerificationReceiptTx(ctx context.Context, tx *sql.Tx, fid, receiptID int64) (*VerificationReceipt, error) {
	v := &VerificationReceipt{}
	var output string
	err := tx.QueryRowContext(ctx, `SELECT er.approval_id,er.version,er.task_id,er.intent_id,er.run_id,er.tool_use_id,er.input_digest,er.state,
CASE WHEN octet_length(er.output)<=1048576 THEN er.output ELSE '' END,
NOT er.output_truncated AND octet_length(er.output) BETWEEN 1 AND 1048576
FROM tool_execution_receipts er JOIN intercept_pending ip ON ip.id=er.approval_id
JOIN findings f ON f.id=$1
WHERE er.approval_id=$2 AND `+verificationReceiptIdentitySQL+`
FOR SHARE OF er,ip`, fid, receiptID).Scan(&v.ID, &v.Version, &v.TaskID, &v.IntentID, &v.RunID, &v.ToolUseID, &v.InputDigest, &v.State, &output, &v.Complete)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(output))
	v.OutputSHA256 = hex.EncodeToString(sum[:])
	return v, nil
}
