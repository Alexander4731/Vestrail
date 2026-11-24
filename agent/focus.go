package agent

import (
	"context"
	"strings"
)

type taskObjectiveKey struct{}

// The immutable objective accompanies every worker continuation, including after
// transcript compaction. It guides relevance; it does not widen tool authority.
func WithTaskObjective(ctx context.Context, objective string) context.Context {
	return context.WithValue(ctx, taskObjectiveKey{}, strings.TrimSpace(objective))
}

func taskObjectiveBlock(ctx context.Context) string {
	objective, _ := ctx.Value(taskObjectiveKey{}).(string)
	if objective == "" {
		return ""
	}
	return "\n\n【任务总目标：用于判断本意图的相关性，不扩大本意图或授权范围】\n" + objective
}

const workerFocusContract = `

【执行与交回条件】
- 每一步围绕本意图及任务总目标，先明确现有证据支持什么假设、这一步能排除什么不确定性。只有名字、版本猜测或宽泛可能性时，先做一次能区分前提真假的验证，不直接投入长链尝试。
- **起跑范围自检与复用**：若交底指定了明确 scope（目标/端口段/候选集）或上游产物路径，先直接复用已验证有效（valid=true）的产物，不要递归翻扫历史目录；复用已有脚本或后台 driver 前先核对其实际起止范围与当前 scope 一致，不一致不得直接核销覆盖。
- **批次有效性与来源严谨**：分批脚本复用历史结果文件时，必须检查文件内容可读、范围匹配且 valid=true && precondition_ok=true，**绝不因文件存在就 SKIP 无效批次**；禁止把历史命令日志（history_log）或猜测字典（guess_wordlist）中的字符串直接升级为目标暴露证据。
- **局部观测不作过度排除**：单个控制端口或健康检查正常仅证明该条路径此时可用，不能单独证明其他失败“不是代理/通道问题”；未做对照实验的解释写 confidence=inferred 并列明未排除假设。
- 同一假设连续三次验证没有得到新证据，或必要前提已被否定，就记录已试方法、真实结果、缺失前提和可替代方向，交回规划者。只改措辞、参数排列或重复同一失败不算新证据。
- 若发现与总目标无关或已由其他意图完成，说明依据并交回；不要自行扩大范围。只有出现新的可核验证据、前提变化或明确纠偏指令，才重新进入已否定方向。
- 收尾保留成功、失败和未知的区别，中间事实写 record_fact；所有结束情形统一调用 finish_work(delivery_status, summary, evidence_ids, checkpoint)，提交已完成范围、剩余范围、有效产物路径和下一条最小动作，作为唯一显式结束动作。这里的交回条件适用于本轮执行；通用的“继续尝试/覆盖所有方法”表述不能取消它。任务授权、人工暂停和工具审批仍然有效。
`

const plannerFocusContract = `

【方向选择与止损】
- 每条新意图在 summary 中简要写清：推进哪个目标、基于什么已有证据、预期验证信号，以及什么结果出现时停止或换路。前置缺失时先派验证前提的最小步骤，不派依赖尚不存在产出的下游步骤。
- 优先能减少关键不确定性、验证成本合理且有证据支持的方向；不要为凑漏洞数量、覆盖率或理论可能性反复投入同一点。失败及否定证据同样更新路线。
- 对 blocked/exhausted/stopped 意图先查实际输出。新意图若复用同一目标、前提和方法，必须有新证据或前提变化；仅换措辞不构成新方向。
- 在跑的 Worker 长期重复失败时用 steer_work 给出具体纠偏；纠偏后仍无新增依据时结束该意图并换路。保留其他有进展的意图。
- 目标未满足但确实缺少外部输入时说明具体缺口；不要把等待、没有新思路或一个方向失败当成目标已完成。
`
