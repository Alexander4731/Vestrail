package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
)

var intentCreationLocks sync.Map

func intentCreationLock(id int64) *sync.Mutex {
	lock, _ := intentCreationLocks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func normalizedIntentText(text string) string { return kernel.NormalizedIntentText(text) }
func intentSimilarity(a, b string) float64    { return kernel.IntentSimilarity(a, b) }

func intentsOverlap(in intentItem, existing *db.Node) (bool, float64) {
	var old intentItem
	if json.Unmarshal(existing.Payload, &old) != nil {
		return false, 1
	}
	return kernel.CheckIntentOverlap(
		kernel.DedupCandidate{
			Summary:  in.Summary,
			Brief:    in.Brief,
			Scope:    in.Scope,
			AssetIDs: pidList(in.AssetIDs),
		},
		kernel.DedupCandidate{
			ID:       existing.ID,
			Summary:  old.Summary,
			Brief:    old.Brief,
			Scope:    old.Scope,
			AssetIDs: pidList(old.AssetIDs),
		},
	)
}

func (t *ToolSet) checkIntentDuplicate(in intentItem) error {
	open, err := t.ts.IntentsInState("open")
	if err != nil {
		return err
	}
	running, err := t.ts.IntentsInState("running")
	if err != nil {
		return err
	}
	for _, existing := range append(open, running...) {
		overlap, threshold := intentsOverlap(in, existing)
		var old intentItem
		_ = json.Unmarshal(existing.Payload, &old)
		if overlap && intentSimilarity(in.Summary, old.Summary) >= threshold {
			return fmt.Errorf("重复意图 #%d 已为 %s；复用其结果或说明不同目标/范围/机理", existing.ID, existing.State)
		}
	}
	// Explicit continuation of an exhausted intent is legitimate handoff.
	if contID := pid(in.ContinueIntentID); contID > 0 {
		if up, err := t.ts.GetNode(contID); err == nil && up != nil && up.Kind == db.KindIntent && up.State == "exhausted" {
			receipt, err := t.ts.LatestIntentReceipt(context.Background(), contID)
			if err != nil {
				return err
			}
			fresh, err := t.ts.FreshParentEvidence([]int64{contID}, pidList(in.ParentIDs))
			if err != nil {
				return err
			}
			if fresh[contID] {
				return nil
			}
			if receipt != nil && receipt.HasNetGain() && continuationCoversRemaining(in.Scope, receipt) {
				return nil
			}
			if (receipt == nil || !receipt.HasNetGain()) && strings.TrimSpace(in.Scope) != "" {
				var previous intentItem
				if json.Unmarshal(up.Payload, &previous) == nil && !kernel.SameCoverageScope(in.Scope, previous.Scope) {
					return nil
				}
			}
			if receipt != nil && receipt.HasNetGain() {
				return fmt.Errorf("意图 #%d 接力须限定 checkpoint.remaining_scope 的未完成部分；重新验证已覆盖范围须提供新证据", contID)
			}
			return fmt.Errorf("意图 #%d 上轮没有可核验增量；接力须提供新证据或不同的执行范围", contID)
		}
	}

	anchors := pidList(in.AssetIDs)
	doneIntents, err := t.ts.RecentDoneIntentsForDedup(anchors, 200)
	if err != nil {
		return err
	}
	if len(doneIntents) == 0 {
		return nil
	}
	doneIDs := make([]int64, 0, len(doneIntents))
	for _, n := range doneIntents {
		doneIDs = append(doneIDs, n.ID)
	}
	newParents := pidList(in.ParentIDs)
	freshParents, err := t.ts.FreshParentEvidence(doneIDs, newParents)
	if err != nil {
		return err
	}

	for _, existing := range doneIntents {
		overlap, threshold := intentsOverlap(in, existing)
		var old intentItem
		_ = json.Unmarshal(existing.Payload, &old)
		if !overlap || intentSimilarity(in.Summary, old.Summary) < max(0.72, threshold) {
			continue
		}
		// Reentry needs evidence created after completion or a newer consumed version.
		if freshParents[existing.ID] {
			continue
		}
		return fmt.Errorf("意图 #%d 已完成；重入须提供完成后新增或版本更新的事实 parent_ids、或明确不同的 scope/机理", existing.ID)
	}
	return nil
}
