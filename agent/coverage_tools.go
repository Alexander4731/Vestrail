package agent

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/Alexander4731/Vestrail/db"
	actool "github.com/Autumn-27/norma/tool"
)

func (t *ToolSet) listCoverageChecks() actool.CoreTool {
	return t.readExpTool("list_coverage_checks", "读取声明的测试检查计划、结果与缺口。资产触达率不代表测试完成；自报结果不会变成机器已观察。", obj(map[string]any{"after": str("可选，上一页 next_after")}), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var args struct {
			After string `json:"after"`
		}
		if err := json.Unmarshal(in, &args); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		var after int64
		if args.After != "" {
			var err error
			after, err = strconv.ParseInt(args.After, 10, 64)
			if err != nil || after <= 0 {
				return actool.Errorf("invalid cursor"), nil
			}
		}
		out, err := t.ts.TaskCoverageLedger(ctx, after)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(out)
	})
}

func (t *ToolSet) recordCoverageCheck() actool.CoreTool {
	return t.writeExpTool("record_coverage_check", "记录本任务检查项的观察或阻塞原因，保留缺口。reported_no_issue 仅为自报未发现，不能据此宣称目标安全或测试已完成。", obj(map[string]any{
		"check_id": str("当前计划中的检查 ID"), "request_key": str("本次尝试的稳定唯一键；重试沿用"),
		"outcome": map[string]any{"type": "string", "enum": []string{"reported_no_issue", "finding", "blocked", "needs_review"}},
		"reason":  str("实际执行、观察、前提状态或阻塞原因"), "evidence_node_id": str("可选，本任务事实或发现节点 ID"),
	}, "check_id", "request_key", "outcome", "reason"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var args struct {
			CheckID    json.RawMessage `json:"check_id"`
			RequestKey string          `json:"request_key"`
			Outcome    string          `json:"outcome"`
			Reason     string          `json:"reason"`
			EvidenceID json.RawMessage `json:"evidence_node_id"`
		}
		if err := json.Unmarshal(in, &args); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if pid(args.CheckID) <= 0 || len(args.EvidenceID) > 0 && string(args.EvidenceID) != "null" && pid(args.EvidenceID) <= 0 {
			return actool.Errorf("invalid check/evidence id"), nil
		}
		err := t.ts.RecordCheckReport(ctx, pid(args.CheckID), "agent:"+t.worker, db.CoverageAttemptInput{RequestKey: args.RequestKey, Outcome: args.Outcome, Reason: args.Reason, EvidenceNodeID: pid(args.EvidenceID)})
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		return jsonResult(map[string]any{"recorded": true, "authority": "agent_reported"})
	})
}
