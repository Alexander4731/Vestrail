package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Alexander4731/Vestrail/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) hintViews(intent *db.Node, limit int) ([]map[string]any, int64, int64, error) {
	hints, resolved, active, err := t.ts.ActiveHints(1000)
	if err != nil {
		return nil, 0, 0, err
	}
	ids := make([]int64, 0, len(hints))
	for _, h := range hints {
		ids = append(ids, h.ID)
	}
	assets, err := t.ts.NodeAssets(ids)
	if err != nil {
		return nil, 0, 0, err
	}
	wanted := intentAssetIDs(intent)
	type item struct {
		node *db.Node
		rank int
	}
	items := make([]item, 0, len(hints))
	for _, h := range hints {
		rank := 1
		if len(assets[h.ID]) > 0 {
			rank = 2
			for _, target := range wanted {
				for _, asset := range assets[h.ID] {
					if target == asset {
						rank = 0
					}
				}
			}
		}
		if len(wanted) > 0 && rank == 2 {
			continue
		}
		items = append(items, item{h, rank})
	}
	if intent != nil {
		sort.SliceStable(items, func(i, j int) bool { return items[i].rank < items[j].rank })
	}
	if len(items) > limit {
		items = items[:limit]
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		var p map[string]any
		_ = json.Unmarshal(item.node.Payload, &p)
		m := map[string]any{"id": item.node.ID, "state": item.node.State, "text": truncRuneSummary(p["text"], 200)}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			m["traffic_refs"] = p["traffic_refs"]
		}
		out = append(out, m)
	}
	return out, resolved, active, nil
}

func (t *ToolSet) resolveHint() actool.CoreTool {
	return t.writeExpTool("resolve_hint", "将本任务已处理或已排除的提示标为 resolved，保留原文与处理原因。", obj(map[string]any{"hint_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1}}, "reason": str("处理依据")}, "hint_ids"),
		func(_ context.Context, raw json.RawMessage) (actool.Result, error) {
			var in struct {
				HintIDs []int64 `json:"hint_ids"`
				Reason  string  `json:"reason"`
			}
			if err := json.Unmarshal(raw, &in); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if len(in.HintIDs) == 0 {
				return actool.Errorf("hint_ids 不能为空"), nil
			}
			if err := t.ts.ResolveHints(in.HintIDs, in.Reason); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("resolved %d hint(s)", len(in.HintIDs))), nil
		})
}
