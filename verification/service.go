// Package verification checks persisted evidence without dispatching tools or
// requests to a target. Only host implementations can write their conclusions.
package verification

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/evidence"
)

const maxBodyBytes int64 = 16 << 20
const maxTotalBytes int64 = 64 << 20
const maxMetadataBytes int64 = 2 << 20
const maxBindings = 100

type Descriptor struct {
	Kind            string `json:"kind"`
	Version         int    `json:"version"`
	Assurance       string `json:"assurance"`
	Description     string `json:"description"`
	ExternalActions bool   `json:"external_actions"`
}

func Registry() []Descriptor {
	return []Descriptor{
		{Kind: "evidence_integrity", Version: 1, Assurance: "evidence_bound", Description: "核对绑定流量的元数据、原始文件哈希与长度；不判定漏洞成立"},
		{Kind: "execution_receipt", Version: 1, Assurance: "evidence_bound", Description: "核对本发现来源意图的宿主执行回执与完整输出；不判定漏洞成立"},
	}
}

type Request struct {
	RequestKey      string `json:"request_key"`
	EvidenceVersion int64  `json:"evidence_version"`
	Verifier        string `json:"verifier"`
	ReceiptID       int64  `json:"receipt_id,string,omitempty"`
}

type Service struct{ Store *evidence.Store }

// Run has no detached execution. A dropped connection seals an inconclusive
// attempt using a short independent context; restart recovery handles DB loss.
func (s Service) Run(ctx context.Context, fid int64, actor string, req Request) (result *db.VerificationAttempt, created bool, err error) {
	if req.Verifier != "evidence_integrity" && req.Verifier != "execution_receipt" {
		return nil, false, db.ErrVerificationInput
	}
	if req.Verifier == "execution_receipt" && req.ReceiptID <= 0 || req.Verifier == "evidence_integrity" && req.ReceiptID != 0 {
		return nil, false, db.ErrVerificationInput
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	raw, _ := json.Marshal(req)
	a, created, err := s.Store.DB.BeginVerification(ctx, fid, req.EvidenceVersion, req.RequestKey, req.Verifier, actor, raw)
	if err != nil || !created {
		return a, created, err
	}
	sealed := false
	defer func() {
		if sealed {
			return
		}
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer done()
		sealErr := s.Store.DB.FinishVerification(cleanup, a.ID, db.VerificationDecision{Outcome: "inconclusive", Assurance: "none", Reason: "验证中断或读取失败，请检查材料后重试。", Limitations: []string{"verification_interrupted"}})
		err = errors.Join(err, sealErr)
	}()
	v := db.VerificationDecision{Outcome: "inconclusive", Assurance: "none", Limitations: []string{}}
	err = s.Store.DB.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		if err := db.LockFindingEvidenceTx(ctx, tx, fid, &req.EvidenceVersion); err != nil {
			return err
		}
		if req.Verifier == "execution_receipt" {
			r, err := db.VerificationReceiptTx(ctx, tx, fid, req.ReceiptID)
			if errors.Is(err, sql.ErrNoRows) {
				v.Reason = "未找到属于本发现来源意图的权威执行回执。"
				v.Limitations = []string{"receipt_unbound"}
				return nil
			}
			if err != nil {
				return err
			}
			v.Receipt = r
			if r.State != "succeeded" || !r.Complete {
				v.Reason = "回执未成功完成或仅有不完整输出。"
				v.Limitations = []string{"receipt_incomplete"}
				return nil
			}
			v.Outcome = "supported"
			v.Assurance = "evidence_bound"
			v.Reason = "执行身份、来源意图、回执版本及完整输出已核对。该结果仅证明证据来源。"
			return nil
		}
		count, metadataBytes, err := db.FindingTrafficUsageTx(ctx, tx, fid, maxBindings+1)
		if err != nil {
			return err
		}
		if count == 0 {
			v.Reason = "尚未绑定流量证据，无法检查文件完整性。"
			v.Limitations = []string{"no_bound_traffic"}
			return nil
		}
		if count > maxBindings {
			v.Reason = "证据数量超过本次检查上限。"
			v.Limitations = []string{"binding_limit"}
			return nil
		}
		if metadataBytes > maxMetadataBytes {
			v.Reason = "证据元数据超过本次检查上限。"
			v.Limitations = []string{"metadata_limit"}
			return nil
		}
		traffic, err := db.FindingTrafficTx(tx, fid)
		if err != nil {
			return err
		}
		var total int64
		type bodyKey struct {
			hash   string
			length int64
		}
		checked := map[bodyKey]bool{}
		refs := make([]map[string]any, 0, len(traffic.Bindings))
		for _, b := range traffic.Bindings {
			if err = ctx.Err(); err != nil {
				return err
			}
			snapshot := b.Snapshot
			if snapshot.ID != db.TrafficSnapshotID(snapshot) {
				v.Reason = "证据元数据哈希不匹配。"
				v.Limitations = []string{"metadata_mismatch"}
				return nil
			}
			if snapshot.ReqLen < 0 || snapshot.RespLen < 0 || snapshot.ReqLen > maxBodyBytes || snapshot.RespLen > maxBodyBytes {
				v.Reason = "证据大小超出单文件检查限制。"
				v.Limitations = []string{"body_limit"}
				return nil
			}
			for _, body := range []bodyKey{{snapshot.ReqHash, snapshot.ReqLen}, {snapshot.RespHash, snapshot.RespLen}} {
				if checked[body] {
					continue
				}
				total += body.length
				if total > maxTotalBytes {
					v.Reason = "证据总量超出本次检查限制。"
					v.Limitations = []string{"total_limit"}
					return nil
				}
				if err = s.Store.VerifyBody(ctx, body.hash, body.length); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					v.Reason = fmt.Sprintf("证据绑定 #%d 的原始文件缺失或完整性不符。", b.ID)
					v.Limitations = []string{"body_unavailable"}
					return nil
				}
				checked[body] = true
			}
			refs = append(refs, map[string]any{"binding_id": fmt.Sprint(b.ID), "snapshot_id": snapshot.ID, "req_hash": snapshot.ReqHash, "resp_hash": snapshot.RespHash})
		}
		v.EvidenceRefs, _ = json.Marshal(refs)
		v.Outcome = "supported"
		v.Assurance = "evidence_bound"
		v.Reason = "绑定流量的元数据、请求和响应原始文件均通过完整性检查。该结果不判断漏洞是否成立。"
		return nil
	})
	if err != nil {
		return nil, true, err
	}
	if err = s.Store.DB.FinishVerification(ctx, a.ID, v); err != nil {
		return nil, true, err
	}
	sealed = true
	result, err = s.Store.DB.GetVerification(ctx, fid, a.ID)
	return result, true, err
}
