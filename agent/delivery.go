package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

type runEvidenceKey struct{}

type runEvidenceContext struct {
	snapshot func() []kernel.EvidenceBatch
	seed     func([]kernel.EvidenceBatch)
}

func WithRunEvidence(ctx context.Context, snapshot func() []kernel.EvidenceBatch, seed func([]kernel.EvidenceBatch)) context.Context {
	return context.WithValue(ctx, runEvidenceKey{}, runEvidenceContext{snapshot: snapshot, seed: seed})
}

func runEvidence(ctx context.Context) []kernel.EvidenceBatch {
	if evidence, ok := ctx.Value(runEvidenceKey{}).(runEvidenceContext); ok && evidence.snapshot != nil {
		return evidence.snapshot()
	}
	return nil
}

func seedRunEvidence(ctx context.Context, batches []kernel.EvidenceBatch) {
	if evidence, ok := ctx.Value(runEvidenceKey{}).(runEvidenceContext); ok && evidence.seed != nil {
		evidence.seed(batches)
	}
}

func VerifyArtifact(ctx context.Context, dir string, ref kernel.ArtifactRef) (kernel.ArtifactRef, []kernel.EvidenceBatch, error) {
	path := ref.Path
	if !filepath.IsAbs(path) {
		if dir == "" || !filepath.IsLocal(path) {
			return ref, nil, fmt.Errorf("产物必须提供可定位的文件路径")
		}
		path = filepath.Join(dir, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return ref, nil, fmt.Errorf("产物不可读: %s", path)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return ref, nil, fmt.Errorf("产物必须是非空普通文件: %s", path)
	}
	h := sha256.New()
	var preview bytes.Buffer
	reader := bufio.NewReaderSize(io.TeeReader(f, h), 32*1024)
	const previewLimit = 8 * 1024 * 1024
	line := []byte{}
	lineTooLong, lineHasSchema := false, false
	batches := []kernel.EvidenceBatch{}
	var parseErr error
	var bytesRead int64
	for {
		if err := ctx.Err(); err != nil {
			return ref, nil, err
		}
		fragment, readErr := reader.ReadSlice('\n')
		bytesRead += int64(len(fragment))
		if remaining := previewLimit - preview.Len(); remaining > 0 {
			preview.Write(fragment[:min(remaining, len(fragment))])
		}
		if ref.Provenance != "source_code" {
			lineHasSchema = lineHasSchema || bytes.Contains(fragment, []byte(kernel.BatchSchemaV1))
			if len(line)+len(fragment) <= previewLimit {
				line = append(line, fragment...)
			} else {
				lineTooLong = true
			}
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		trimmed := bytes.TrimSpace(line)
		structured := json.Valid(trimmed) && bytes.HasPrefix(trimmed, []byte("{"))
		if ref.Provenance != "source_code" && (ref.Kind == "batch_log" && len(trimmed) > 0 || structured && lineHasSchema) {
			parsed := kernel.ExtractEvidenceBatches(string(line))
			if lineTooLong || len(parsed) != 1 {
				parseErr = fmt.Errorf("产物批次数据无法解析: %s", path)
			} else if batch := parsed[0]; kernel.ValidateBatch(batch) != nil || !batch.Valid || !batch.PreconditionOK {
				parseErr = fmt.Errorf("产物含无效批次: %s", path)
			} else if len(batches) == 0 || !kernel.MergeNegativeBatch(&batches[len(batches)-1], batch) {
				batches = append(batches, batch)
			}
		}
		line, lineTooLong, lineHasSchema = line[:0], false, false
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return ref, nil, readErr
		}
	}
	if bytesRead != info.Size() {
		return ref, nil, fmt.Errorf("产物读取期间大小已变化: %s", path)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if ref.Digest != "" && ref.Digest != digest {
		return ref, nil, fmt.Errorf("产物内容已变化: %s", path)
	}
	if ref.Provenance != "source_code" {
		var whole kernel.EvidenceBatch
		if info.Size() <= int64(preview.Len()) && json.Unmarshal(preview.Bytes(), &whole) == nil && whole.Schema == kernel.BatchSchemaV1 {
			batches = []kernel.EvidenceBatch{whole}
		} else if info.Size() <= int64(preview.Len()) && json.Valid(preview.Bytes()) && bytes.HasPrefix(bytes.TrimSpace(preview.Bytes()), []byte("[")) {
			var items []kernel.EvidenceBatch
			if err := json.Unmarshal(preview.Bytes(), &items); err == nil {
				batches = nil
				for _, item := range items {
					if item.Schema == kernel.BatchSchemaV1 {
						batches = append(batches, item)
					}
				}
			} else if ref.Kind == "batch_log" {
				return ref, nil, err
			}
		} else if parseErr != nil {
			return ref, nil, parseErr
		}
		if ref.Kind == "batch_log" && len(batches) == 0 {
			return ref, nil, fmt.Errorf("产物缺少有效批次: %s", path)
		}
	}
	for _, batch := range batches {
		if err := kernel.ValidateBatch(batch); err != nil || !batch.Valid || !batch.PreconditionOK {
			return ref, nil, fmt.Errorf("产物含无效批次: %s", path)
		}
	}
	ref.Path, ref.Digest, ref.Size, ref.Valid = path, digest, info.Size(), true
	return ref, batches, nil
}

func ValidateScopeBatches(scope string, batches []kernel.EvidenceBatch) error {
	ranges := kernel.PortScopeRanges(scope)
	if len(ranges) == 0 {
		return nil
	}
	for _, b := range batches {
		if b.RangeStart == 0 && b.RangeEnd == 0 {
			continue
		}
		if !b.Valid || !b.PreconditionOK || b.UniqueCovered <= 0 {
			continue
		}
		contained := false
		for _, bounds := range ranges {
			if b.RangeStart >= int64(bounds[0]) && b.RangeEnd <= int64(bounds[1]) {
				contained = true
				break
			}
		}
		if !contained {
			return fmt.Errorf("批次范围 %d–%d 超出意图声明的端口范围", b.RangeStart, b.RangeEnd)
		}
	}
	return nil
}

func CompleteScopeCovered(scope string, batches []kernel.EvidenceBatch) bool {
	tracker := kernel.NewCoverageTracker(scope)
	tracker.Seed(batches)
	for _, bounds := range kernel.PortScopeRanges(scope) {
		if !tracker.CoversRange(int64(bounds[0]), int64(bounds[1])) {
			return false
		}
	}
	return true
}

func (t *ToolSet) noteEvidenceRead(ctx context.Context, ids []int64) {
	versions, err := t.ts.EvidenceVersions(ctx, ids)
	if err != nil {
		return
	}
	t.updateWrites(func(w *WriteCounts) {
		if w.ConsumedEvidenceVersions == nil {
			w.ConsumedEvidenceVersions = map[int64]int{}
		}
		for id, version := range versions {
			w.ConsumedEvidenceVersions[id] = version
		}
	})
}

func (t *ToolSet) finishWork() actool.CoreTool {
	return t.writeExpTool("finish_work", "提交本意图的明确交付回执。完成用 complete；已有部分结果用 partial_checkpoint；前提不成立用 precondition_blocked；尚未完成交回用 handed_back。checkpoint 允许零新事实；引用有效证据、已登记资产或核验后的产物。", obj(map[string]any{
		"delivery_status": str("complete | partial_checkpoint | precondition_blocked | handed_back"),
		"summary":         str("本次交付及未完成部分的简短说明"),
		"evidence_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "支撑交付的 fact/finding 节点；省略时采用本次写回的证据"},
		"checkpoint":      map[string]any{"type": "object", "description": "{completed_scope,remaining_scope,next_action,valid_artifacts,invalid_artifacts}"},
	}, "delivery_status", "summary"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		var req struct {
			Status      kernel.DeliveryStatus `json:"delivery_status"`
			Summary     string                `json:"summary"`
			EvidenceIDs []int64               `json:"evidence_ids"`
			Checkpoint  *kernel.Checkpoint    `json:"checkpoint"`
		}
		if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.Summary) == "" {
			return actool.Errorf("必须提供交付状态和简短说明"), nil
		}
		switch req.Status {
		case kernel.DeliveryComplete, kernel.DeliveryPartialCheckpoint, kernel.DeliveryPreconditionBlocked, kernel.DeliveryHandedBack:
		default:
			return actool.Errorf("未知交付状态"), nil
		}
		written := t.Writes()
		if req.Checkpoint == nil {
			req.Checkpoint = written.Checkpoint
		}
		if len(req.EvidenceIDs) == 0 {
			req.EvidenceIDs = written.EvidenceIDs
		}
		for _, id := range req.EvidenceIDs {
			node, err := t.ts.GetNodeWithSources(id)
			if err != nil || node == nil || (node.Kind != db.KindFact && node.Kind != db.KindFinding) || node.State != "confirmed" {
				return actool.Errorf("evidence_ids 必须引用可用的事实或发现"), nil
			}
		}
		t.noteEvidenceRead(ctx, req.EvidenceIDs)
		batches := runEvidence(ctx)
		artifacts := 0
		if req.Checkpoint != nil {
			refs := req.Checkpoint.ValidArtifacts
			req.Checkpoint.ValidArtifacts = nil
			for _, ref := range refs {
				verified, batch, err := VerifyArtifact(ctx, t.artifactDir, ref)
				if err != nil {
					if req.Status == kernel.DeliveryComplete {
						return actool.Errorf(err.Error()), nil
					}
					req.Checkpoint.InvalidArtifacts = append(req.Checkpoint.InvalidArtifacts, ref.Path)
					continue
				}
				if err := ValidateScopeBatches(t.contract.Scope, batch); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				req.Checkpoint.ValidArtifacts = append(req.Checkpoint.ValidArtifacts, verified)
				batches = append(batches, batch...)
				known := false
				for _, input := range t.contract.Inputs {
					known = known || (input.Digest != "" && input.Digest == verified.Digest)
				}
				if !known {
					artifacts++
				}
			}
		}
		acceptedArtifacts := 0
		if req.Checkpoint != nil {
			acceptedArtifacts = len(req.Checkpoint.ValidArtifacts)
		}
		hasRangeBatch := false
		for _, batch := range batches {
			if batch.RangeStart > 0 || batch.RangeEnd > 0 {
				hasRangeBatch = true
				break
			}
		}
		requiresBatchCoverage := hasRangeBatch || strings.Contains(strings.ToLower(t.contract.Deliverable), kernel.BatchSchemaV1)
		if req.Status == kernel.DeliveryComplete && (len(req.EvidenceIDs) == 0 && acceptedArtifacts == 0 && written.Assets == 0 || requiresBatchCoverage && !CompleteScopeCovered(t.contract.Scope, batches)) {
			return actool.Errorf("最小交付尚缺证据、有效产物或完整覆盖，请提交 partial_checkpoint / handed_back"), nil
		}
		if ctx.Err() != nil {
			return actool.Errorf("交付已超过执行预算，请保留现有证据后重试"), nil
		}
		t.updateWrites(func(w *WriteCounts) {
			w.DeliveryStatus = req.Status
			if req.Checkpoint != nil {
				w.Checkpoint = req.Checkpoint
			}
			seen := map[int64]bool{}
			for _, id := range w.EvidenceIDs {
				seen[id] = true
			}
			for _, id := range req.EvidenceIDs {
				if !seen[id] {
					w.EvidenceIDs = append(w.EvidenceIDs, id)
					seen[id] = true
				}
			}
			w.DeliveryFinalized, w.FinalText = true, req.Summary
			w.ValidBatches = append([]kernel.EvidenceBatch(nil), batches...)
			w.ResearchGain.Artifacts = max(w.ResearchGain.Artifacts, artifacts)
		})
		latest := t.Writes()
		finalGain := latest.ResearchGain
		finalGain.Assets = latest.Assets
		harness.RequestCompletion(ctx, req.Summary)
		return jsonResult(map[string]any{"delivery_status": req.Status, "checkpoint": latest.Checkpoint, "research_gain": finalGain})
	})
}

type workerDeliveryHooks struct {
	harness.HookRunner
	tools     *ToolSet
	requested bool
}

func (h *workerDeliveryHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	if h.tools.Writes().DeliveryFinalized {
		return true, "本意图已提交明确交付回执", nil
	}
	if h.HookRunner != nil {
		return h.HookRunner.PreToolUse(ctx, name, input)
	}
	return false, "", nil
}

func (h *workerDeliveryHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.HookRunner != nil {
		h.HookRunner.PostToolUse(ctx, name, input, result, isErr)
	}
}

func (h *workerDeliveryHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if h.HookRunner != nil {
		if stop, blockers, message := h.HookRunner.Stop(ctx, messages); stop || len(blockers) > 0 || message != "" {
			return stop, blockers, message
		}
	}
	if !h.requested && !h.tools.Writes().DeliveryFinalized {
		h.requested = true
		return false, []string{"请现在调用 finish_work 提交明确交付回执；未完成时提交剩余范围与下一步，允许零新事实。"}, ""
	}
	return false, nil, ""
}
