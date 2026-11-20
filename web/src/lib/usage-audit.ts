import type { TaskUsageAudit, UsageAuditGroup, UsageAuditStat } from "./types";

export function firstTurnInputP50Value(stat: UsageAuditStat): number | null {
  const value = stat.first_turn_input_p50;
  return (stat.first_turn_input_rows ?? 0) > 0 && value !== undefined && Number.isFinite(value) ? value : null;
}

function cell(value: string | number): string {
  return String(value)
    .replaceAll("|", "\\|")
    .replace(/[\r\n]+/g, " ");
}

function groupTable(title: string, groups: UsageAuditGroup[]): string {
  const lines = [
    `## ${title}`,
    "",
    "| 分组 | 记录 | 会话数 | 首条输入 P50 | 输入 | 缓存读 | 缓存写 | 未命中输入 | 输出 | 总处理量 | 输入 P95 |",
    "| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
    ...groups.map((g) =>
      [
        g.key,
        g.rows,
        g.distinct_sessions ?? "—",
        firstTurnInputP50Value(g) ?? "—",
        g.input_tokens,
        g.cache_read_tokens,
        g.cache_write_tokens,
        g.uncached_input_tokens,
        g.output_tokens,
        g.total_tokens,
        g.nonzero_input_rows > 0 ? g.input_p95 : "—",
      ]
        .map(cell)
        .join(" | "),
    ),
    "",
  ];
  return lines.join("\n");
}

// Export the exact displayed snapshot locally; generating this report does not
// request a new model completion or change the task's execution state.
export function usageAuditMarkdown(audit: TaskUsageAudit): string {
  const u = audit.total;
  const lines = [
    `# 任务 ${cell(audit.task_id)} 用量分析`,
    "",
    `- 统计时间（UTC）：${cell(audit.generated_at)}`,
    `- 数据来源：${cell(audit.source)}`,
    `- 范围起点：${audit.since ? cell(audit.since) : "历史累计"}`,
    `- 首条 / 末条记录：${u.first_seen ?? "—"} / ${u.last_seen ?? "—"}`,
    "",
    "## 计量合计",
    "",
    "| 项目 | 数值 |",
    "| --- | ---: |",
    `| 输入（含缓存） | ${u.input_tokens} |`,
    `| 缓存读取 | ${u.cache_read_tokens} |`,
    `| 缓存写入 | ${u.cache_write_tokens} |`,
    `| 未命中输入 | ${u.uncached_input_tokens} |`,
    `| 输出 | ${u.output_tokens} |`,
    `| 总处理量（输入 + 输出） | ${u.total_tokens} |`,
    `| 非零输入 P50 / P95 | ${u.nonzero_input_rows > 0 ? `${u.input_p50} / ${u.input_p95}` : "—"} |`,
    `| 计量记录 | ${u.rows} |`,
    `| 可识别调用 ID | ${u.known_call_ids} |`,
    `| 已记录会话数 | ${u.distinct_sessions ?? "—"} |`,
    `| 首条输入 P50 | ${firstTurnInputP50Value(u) ?? "—"} |`,
    `| 首条输入样本数 | ${u.first_turn_input_rows ?? "—"} |`,
    `| 缺调用 ID 的记录 | ${u.missing_call_ids} |`,
    `| 标记为重试的记录 | ${u.repeated_attempt_rows} |`,
    `| 错误记录 / 零用量记录 | ${u.error_rows} / ${u.zero_usage_rows} |`,
    `| 缺用途 / 会话 / 角色 | ${u.missing_purpose} / ${u.missing_session} / ${u.missing_worker} |`,
    `| 缓存计量异常记录 | ${u.invalid_cache_rows} |`,
    "",
    "输入已包含缓存读取与写入，不能再把缓存加到总处理量。缓存计量异常时，不应以未命中输入推算费用。",
    "",
    "可识别调用 ID 不等于实际请求数，分组之间也可能重复。标记重试只包括可见的 attempt_id > 1。零用量记录不能证明上游未扣费。实付费用需要供应商账单确认。",
    "",
    "会话数统计所选范围中出现的非空会话 ID，不等于实际冷启动次数，各分组的会话数不能相加。首条输入按任务完整账本定位每个会话的第一条记录；仅其非零输入纳入样本，再按时间范围筛选。缺失或晚开始的历史计量不能用于推断真正的第一轮。",
    "",
    groupTable("按角色", audit.by_worker),
    groupTable("按用途", audit.by_purpose),
    groupTable("按模型", audit.by_model),
  ];
  return lines.join("\n");
}
