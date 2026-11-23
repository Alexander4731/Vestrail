package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/kernel"
)

func (t *ToolSet) normalizeIntentContract(in *intentItem, assets []int64, require bool) error {
	in.Scope, in.Deliverable = strings.TrimSpace(in.Scope), strings.TrimSpace(in.Deliverable)
	if strings.TrimSpace(in.Hypothesis) == "" {
		in.Hypothesis = in.Summary
	}
	if source := pid(in.ContinueIntentID); source > 0 {
		if receipt, err := t.ts.LatestIntentReceipt(context.Background(), source); err != nil {
			return err
		} else if receipt != nil && receipt.Checkpoint != nil {
			if in.Scope == "" {
				in.Scope = strings.TrimSpace(receipt.Checkpoint.RemainingScope)
			}
			if len(in.Inputs) == 0 {
				in.Inputs = append(in.Inputs, receipt.Checkpoint.ValidArtifacts...)
			}
		}
	}
	if in.Scope == "" && len(assets) > 0 {
		sort.Slice(assets, func(i, j int) bool { return assets[i] < assets[j] })
		in.Scope = fmt.Sprintf("asset_ids:%v", assets)
	}
	if in.ToolBudget != nil {
		in.MaxToolCalls = *in.ToolBudget
	}
	missing := []string{}
	if in.Scope == "" {
		missing = append(missing, "scope")
	}
	if in.Deliverable == "" {
		missing = append(missing, "deliverable")
	}
	if in.ToolBudget == nil && in.MaxToolCalls == 0 {
		missing = append(missing, "max_tool_calls（0 明确不限）")
	}
	if require && len(missing) > 0 {
		return fmt.Errorf("执行交底缺少 %s；请一次补齐后提交", strings.Join(missing, ", "))
	}
	if in.MaxToolCalls < 0 {
		return fmt.Errorf("max_tool_calls 必须为非负整数")
	}
	for _, dependency := range in.AfterIntents {
		node, err := t.ts.GetNode(dependency)
		if dependency <= 0 || err != nil || node == nil || node.Kind != db.KindIntent || node.State == "deleted" {
			return fmt.Errorf("after_intents 必须引用本任务已有的有效意图")
		}
	}
	for _, lock := range in.ResourceLocks {
		if strings.TrimSpace(lock.Resource) == "" || (lock.Mode != "shared" && lock.Mode != "exclusive") {
			return fmt.Errorf("resource_locks 必须提供资源标识与 shared/exclusive 模式")
		}
	}
	return nil
}

func continuationCoversRemaining(scope string, receipt *kernel.RunReceipt) bool {
	if receipt == nil || receipt.Checkpoint == nil {
		return false
	}
	remaining := strings.TrimSpace(receipt.Checkpoint.RemainingScope)
	if remaining == "" {
		return false
	}
	if strings.Join(strings.Fields(scope), " ") == strings.Join(strings.Fields(remaining), " ") {
		return true
	}
	start, end, bounded := kernel.PortScopeBounds(scope)
	remainingStart, remainingEnd, remainingBounded := kernel.PortScopeBounds(remaining)
	context, hasContext := kernel.PortScopeContext(scope)
	remainingContext, hasRemainingContext := kernel.PortScopeContext(remaining)
	return bounded && remainingBounded && hasContext && hasRemainingContext && context == remainingContext && start >= remainingStart && end <= remainingEnd
}
