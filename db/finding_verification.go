package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

//go:embed finding_verification.sql
var findingVerificationSchema string

var ErrVerificationConflict = errors.New("验证请求、证据版本或运行状态已变化")
var ErrVerificationInput = errors.New("无效的验证请求")

type VerificationSummary struct {
	State           string `json:"state"`
	Assurance       string `json:"assurance"`
	EvidenceVersion int64  `json:"evidence_version"`
	AttemptID       *int64 `json:"attempt_id,string,omitempty"`
	Stale           bool   `json:"stale"`
}

func (f *DBFinding) Verification() VerificationSummary {
	state, assurance := f.VerificationState, f.VerificationAssurance
	if state == "" {
		state = "legacy_unverified"
	}
	if assurance == "" {
		assurance = "none"
	}
	return VerificationSummary{State: state, Assurance: assurance, EvidenceVersion: f.VerificationEvidenceVersion,
		AttemptID: f.VerificationAttemptID, Stale: f.verificationStale || f.VerificationAttemptID != nil && f.VerificationEvidenceVersion != f.EvidenceVersion}
}

type VerificationAttempt struct {
	ID              int64           `json:"id,string"`
	FindingID       int64           `json:"finding_id,string"`
	RequestKey      string          `json:"request_key"`
	RequestDigest   string          `json:"request_digest"`
	TaskID          string          `json:"task_id"`
	EvidenceVersion int64           `json:"evidence_version"`
	Verifier        string          `json:"verifier"`
	VerifierVersion int             `json:"verifier_version"`
	Actor           string          `json:"actor"`
	State           string          `json:"state"`
	Outcome         string          `json:"outcome"`
	Assurance       string          `json:"assurance"`
	Reason          string          `json:"reason"`
	EvidenceRefs    json.RawMessage `json:"evidence_refs"`
	Limitations     json.RawMessage `json:"limitations"`
	CreatedAt       time.Time       `json:"created_at"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
}

const verificationCols = `id,finding_id,request_key,request_digest,task_id,evidence_version,verifier,verifier_version,actor,state,outcome,assurance,reason,evidence_refs,limitations,created_at,finished_at`

func scanVerification(row interface{ Scan(...any) error }) (*VerificationAttempt, error) {
	v := &VerificationAttempt{}
	err := row.Scan(&v.ID, &v.FindingID, &v.RequestKey, &v.RequestDigest, &v.TaskID, &v.EvidenceVersion, &v.Verifier, &v.VerifierVersion, &v.Actor, &v.State, &v.Outcome, &v.Assurance, &v.Reason, &v.EvidenceRefs, &v.Limitations, &v.CreatedAt, &v.FinishedAt)
	return v, err
}

// BeginVerification records a host-chosen verifier and the exact request. A
// reused key with different content is a conflict, not a successful retry.
func (d *DB) BeginVerification(ctx context.Context, findingID, version int64, key, verifier, actor string, request json.RawMessage) (*VerificationAttempt, bool, error) {
	if findingID <= 0 || version < 0 || len(key) == 0 || len(key) > 128 || len(actor) == 0 || len(actor) > 128 || len(verifier) > 64 || len(request) > 32<<10 || !json.Valid(request) {
		return nil, false, ErrVerificationInput
	}
	if verifier != "evidence_integrity" && verifier != "execution_receipt" && verifier != "controlled_marker" && verifier != "authz_readonly" {
		return nil, false, fmt.Errorf("unknown verifier")
	}
	digest := sha256.Sum256(request)
	hash := hex.EncodeToString(digest[:])
	var out *VerificationAttempt
	created := false
	err := d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := LockFindingEvidenceTx(ctx, tx, findingID, &version); err != nil {
			return err
		}
		var err error
		out, err = scanVerification(tx.QueryRowContext(ctx, `SELECT `+verificationCols+` FROM finding_verification_attempts WHERE finding_id=$1 AND request_key=$2`, findingID, key))
		if err == nil {
			if out.RequestDigest != hash || out.Verifier != verifier || out.EvidenceVersion != version || out.Actor != actor {
				return ErrVerificationConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var running bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM finding_verification_attempts WHERE finding_id=$1 AND state='running')`, findingID).Scan(&running); err != nil {
			return err
		}
		if running {
			return ErrVerificationConflict
		}
		out, err = scanVerification(tx.QueryRowContext(ctx, `INSERT INTO finding_verification_attempts(finding_id,request_key,request_digest,task_id,evidence_version,verifier,actor)
SELECT id,$2,$3,COALESCE(task_id::text,''),evidence_version,$4,$5 FROM findings WHERE id=$1 RETURNING `+verificationCols, findingID, key, hash, verifier, actor))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE findings SET verification_state='running',verification_assurance='none',verification_attempt_id=$2,verification_evidence_version=evidence_version WHERE id=$1`, findingID, out.ID)
		created = err == nil
		return err
	})
	return out, created, err
}

type VerificationDecision struct {
	Outcome      string
	Assurance    string
	Reason       string
	EvidenceRefs json.RawMessage
	Limitations  []string
	Receipt      *VerificationReceipt
}

// FinishVerification is internal to trusted verification implementations. API
// handlers must never decode this type from a model/client verdict.
func (d *DB) FinishVerification(ctx context.Context, attemptID int64, v VerificationDecision) error {
	if v.Outcome != "supported" && v.Outcome != "not_supported" && v.Outcome != "inconclusive" {
		return fmt.Errorf("invalid outcome")
	}
	if v.Assurance != "none" && v.Assurance != "evidence_bound" && v.Assurance != "semantic_verified" {
		return fmt.Errorf("invalid assurance")
	}
	if v.Outcome != "supported" && v.Assurance != "none" {
		return fmt.Errorf("unsupported outcome cannot grant assurance")
	}
	if v.Receipt != nil {
		v.EvidenceRefs, _ = json.Marshal([]*VerificationReceipt{v.Receipt})
	}
	if len(v.EvidenceRefs) == 0 {
		v.EvidenceRefs = json.RawMessage(`[]`)
	}
	if !json.Valid(v.EvidenceRefs) || len(v.EvidenceRefs) > 64<<10 || len(v.Reason) > 16384 {
		return fmt.Errorf("invalid decision")
	}
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var fid int64
		if err := tx.QueryRowContext(ctx, `SELECT finding_id FROM finding_verification_attempts WHERE id=$1`, attemptID).Scan(&fid); err != nil {
			return err
		}
		if err := LockFindingEvidenceTx(ctx, tx, fid, nil); err != nil {
			return err
		}
		a, err := scanVerification(tx.QueryRowContext(ctx, `SELECT `+verificationCols+` FROM finding_verification_attempts WHERE id=$1 FOR UPDATE`, attemptID))
		if err != nil {
			return err
		}
		if a.State != "running" {
			return ErrVerificationConflict
		}
		if a.Verifier == "execution_receipt" && v.Assurance != "none" && (v.Receipt == nil || v.Receipt.State != "succeeded" || !v.Receipt.Complete) {
			return fmt.Errorf("receipt assurance requires a complete successful receipt")
		}
		if v.Assurance == "semantic_verified" && (a.Verifier != "controlled_marker" && a.Verifier != "authz_readonly" || v.Outcome != "supported") {
			return fmt.Errorf("verifier cannot grant semantic assurance")
		}
		var currentVersion int64
		var currentAttempt sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT evidence_version,verification_attempt_id FROM findings WHERE id=$1`, fid).Scan(&currentVersion, &currentAttempt); err != nil {
			return err
		}
		if v.Receipt != nil {
			current, err := VerificationReceiptTx(ctx, tx, fid, v.Receipt.ID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if current == nil || *current != *v.Receipt {
				v.Outcome = "inconclusive"
				v.Assurance = "none"
				v.Limitations = append(v.Limitations, "receipt_changed")
				v.Reason = "执行回执或归属已变化，请重新验证。"
			}
		}
		state := "candidate"
		if v.Outcome == "not_supported" {
			state = "not_reproduced"
		}
		if v.Outcome == "inconclusive" {
			state = "inconclusive"
		}
		if v.Assurance == "semantic_verified" && v.Outcome == "supported" {
			state = "verified"
		}
		if currentVersion != a.EvidenceVersion {
			v.Outcome = "inconclusive"
			v.Assurance = "none"
			state = "inconclusive"
			v.Limitations = append(v.Limitations, "evidence_changed")
			v.Reason = "验证期间证据已变化，请基于新版本重新验证。"
		}
		if v.Limitations == nil {
			v.Limitations = []string{}
		}
		limits, err := json.Marshal(v.Limitations)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE finding_verification_attempts SET state='completed',outcome=$2,assurance=$3,reason=$4,evidence_refs=$5,limitations=$6,finished_at=now() WHERE id=$1`, attemptID, v.Outcome, v.Assurance, utf8Clean(v.Reason), string(jsonbClean(v.EvidenceRefs)), string(jsonbClean(limits)))
		if err != nil {
			return err
		}
		if currentAttempt.Valid && currentAttempt.Int64 == attemptID {
			_, err = tx.ExecContext(ctx, `UPDATE findings SET verification_state=$2,verification_assurance=$3 WHERE id=$1`, fid, state, v.Assurance)
		}
		return err
	})
}

func (d *DB) ListVerifications(ctx context.Context, fid, before int64) ([]VerificationAttempt, error) {
	if fid <= 0 || before < 0 {
		return nil, fmt.Errorf("invalid verification cursor")
	}
	rows, err := d.QueryContext(ctx, `SELECT `+verificationCols+` FROM finding_verification_attempts WHERE finding_id=$1 AND ($2::bigint=0 OR id<$2) ORDER BY id DESC LIMIT 100`, fid, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerificationAttempt{}
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, rows.Err()
}

// Interrupt only attempts from a stopped service. Called after the exclusive
// runtime lease is acquired, never while another instance may still be working.
func (d *DB) InterruptVerifications(ctx context.Context) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `WITH interrupted AS (
UPDATE finding_verification_attempts SET state='interrupted',outcome='inconclusive',assurance='none',reason='服务重启，验证未完成',finished_at=now() WHERE state='running' RETURNING id,finding_id)
UPDATE findings f SET verification_state='inconclusive',verification_assurance='none' FROM interrupted i WHERE f.id=i.finding_id AND f.verification_attempt_id=i.id`)
		return err
	})
}

func (d *DB) GetVerification(ctx context.Context, fid, id int64) (*VerificationAttempt, error) {
	return scanVerification(d.QueryRowContext(ctx, `SELECT `+verificationCols+` FROM finding_verification_attempts WHERE finding_id=$1 AND id=$2`, fid, id))
}
