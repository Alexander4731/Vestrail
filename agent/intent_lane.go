package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Alexander4731/Vestrail/db"
)

type intentLane struct {
	Continue    int64
	Root        int64
	Depth       int
	Handoff     int64
	HandoffRoot int64
}

func readIntentLane(n *db.Node) intentLane {
	var p map[string]json.RawMessage
	if n == nil || json.Unmarshal(n.Payload, &p) != nil {
		return intentLane{}
	}
	root := pid(p["effective_lane_root_id"])
	if root == 0 {
		root = pid(p["lane_root_id"])
	}
	depth := int(pid(p["effective_lane_depth"]))
	if depth == 0 {
		depth = int(pid(p["lane_depth"]))
	}
	return intentLane{pid(p["continue_intent_id"]), root, depth, pid(p["handoff_from_intent_id"]), pid(p["handoff_run_dir_intent_id"])}
}

func IntentLaneRoot(n *db.Node) int64 {
	if lane := readIntentLane(n); lane.Root > 0 {
		return lane.Root
	}
	if n != nil {
		return n.ID
	}
	return 0
}

func (t *ToolSet) assignIntentLane(in intentItem, payload map[string]any, anchors, parents []int64) error {
	if !OptimizationEnabled("VESTRAIL_OPT_LANE_WORKER") {
		return nil
	}
	upstreamID := pid(in.ContinueIntentID)
	explicit := upstreamID > 0
	if !explicit && len(parents) == 1 {
		edges, err := t.ts.Edges(5000)
		if err != nil {
			return err
		}
		for _, edge := range edges {
			if edge.Rel == db.RelYields && edge.To == parents[0] && edge.From > upstreamID {
				upstreamID = edge.From
			}
		}
	}
	if upstreamID == 0 {
		return nil
	}
	upstream, err := t.ts.GetNode(upstreamID)
	if err != nil {
		return err
	}
	if upstream == nil || upstream.Kind != db.KindIntent || (upstream.State != "done" && upstream.State != "exhausted") {
		if explicit {
			return fmt.Errorf("continue_intent_id 必须为本任务 done/exhausted 的意图")
		}
		return nil
	}
	if !explicit {
		assets, err := t.ts.NodeAssets([]int64{upstreamID})
		if err != nil {
			return err
		}
		shared := false
		for _, a := range anchors {
			for _, b := range assets[upstreamID] {
				if a == b {
					shared = true
				}
			}
		}
		if !shared {
			return nil
		}
	}
	old := readIntentLane(upstream)
	root, depth := old.Root, old.Depth
	if root <= 0 {
		root = upstreamID
	}
	if depth <= 0 {
		depth = 1
	}
	rootNode, err := t.ts.GetNode(root)
	if err != nil {
		return err
	}
	if rootNode == nil || rootNode.Kind != db.KindIntent {
		if explicit {
			return fmt.Errorf("上游执行线根不可用")
		}
		return nil
	}
	if depth >= 4 {
		payload["handoff_from_intent_id"], payload["handoff_run_dir_intent_id"] = upstreamID, root
		return nil
	}
	payload["continue_intent_id"], payload["lane_root_id"], payload["lane_depth"] = upstreamID, root, depth+1
	return nil
}
