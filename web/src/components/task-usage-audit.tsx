"use client";

import * as React from "react";

import { DownloadIcon, RefreshCwIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import { api } from "@/lib/api";
import type { TaskUsageAudit, UsageAuditGroup } from "@/lib/types";
import { firstTurnInputP50Value, usageAuditMarkdown } from "@/lib/usage-audit";

function firstInput(stat: UsageAuditGroup | TaskUsageAudit["total"]): string {
  const value = firstTurnInputP50Value(stat);
  return value === null ? "—" : tokens(value);
}

function tokens(value: number): string {
  return value.toLocaleString(undefined, { maximumFractionDigits: 1 });
}

function label(key: string): string {
  return key === "(unknown)" ? "未标注" : key;
}

function UsageGroups({ groups }: { groups: UsageAuditGroup[] }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[900px] whitespace-nowrap text-sm">
        <thead>
          <tr className="border-b text-left text-muted-foreground text-xs">
            <th className="py-2 pr-3 font-medium">分组</th>
            <th className="py-2 pr-3 text-right font-medium">记录</th>
            <th className="py-2 pr-3 text-right font-medium">会话数</th>
            <th className="py-2 pr-3 text-right font-medium">首条输入 P50</th>
            <th className="py-2 pr-3 text-right font-medium">总处理量</th>
            <th className="py-2 pr-3 text-right font-medium">未命中输入</th>
            <th className="py-2 pr-3 text-right font-medium">缓存读</th>
            <th className="py-2 pr-3 text-right font-medium">输出</th>
            <th className="py-2 text-right font-medium">输入 P95</th>
          </tr>
        </thead>
        <tbody>
          {groups.map((g) => (
            <tr key={g.key} className="border-b last:border-0">
              <td className="max-w-48 truncate py-2 pr-3 font-mono text-xs" title={g.key}>
                {label(g.key)}
              </td>
              <td className="py-2 pr-3 text-right tabular-nums">{tokens(g.rows)}</td>
              <td className="py-2 pr-3 text-right tabular-nums">
                {g.distinct_sessions === undefined ? "—" : tokens(g.distinct_sessions)}
              </td>
              <td className="py-2 pr-3 text-right tabular-nums">{firstInput(g)}</td>
              <td className="py-2 pr-3 text-right tabular-nums">{tokens(g.total_tokens)}</td>
              <td className="py-2 pr-3 text-right tabular-nums">{tokens(g.uncached_input_tokens)}</td>
              <td className="py-2 pr-3 text-right tabular-nums">{tokens(g.cache_read_tokens)}</td>
              <td className="py-2 pr-3 text-right tabular-nums">{tokens(g.output_tokens)}</td>
              <td className="py-2 text-right tabular-nums">{g.nonzero_input_rows > 0 ? tokens(g.input_p95) : "—"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function TaskUsageAuditCard({ taskId }: { taskId: string }) {
  const [hours, setHours] = React.useState(0);
  const [revision, setRevision] = React.useState(0);
  const [dimension, setDimension] = React.useState<"by_worker" | "by_purpose" | "by_model">("by_worker");
  const requestKey = `${taskId}:${hours}:${revision}`;
  const [state, setState] = React.useState<{
    key: string;
    loading: boolean;
    audit: TaskUsageAudit | null;
    error: string;
  }>({ key: "", loading: true, audit: null, error: "" });

  React.useEffect(() => {
    let alive = true;
    setState({ key: requestKey, loading: true, audit: null, error: "" });
    void api.taskUsageAudit(taskId, hours).then(
      (audit) => {
        if (alive) setState({ key: requestKey, loading: false, audit, error: "" });
      },
      (error: unknown) => {
        if (alive) {
          setState({
            key: requestKey,
            loading: false,
            audit: null,
            error: error instanceof Error ? error.message : "用量分析加载失败",
          });
        }
      },
    );
    return () => {
      alive = false;
    };
  }, [taskId, hours, requestKey]);

  const loading = state.key !== requestKey || state.loading;
  const audit = state.key === requestKey ? state.audit : null;
  const error = state.key === requestKey ? state.error : "";
  const total = audit?.total;

  const exportReport = () => {
    if (!audit) return;
    const url = URL.createObjectURL(new Blob([usageAuditMarkdown(audit)], { type: "text/markdown;charset=utf-8" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `task-${taskId}-usage-audit.md`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-3">
          <CardTitle className="text-base">任务用量分析</CardTitle>
          <div className="flex items-center gap-2">
            <NativeSelect
              aria-label="用量统计范围"
              value={hours}
              onChange={(event) => setHours(Number(event.target.value))}
            >
              <NativeSelectOption value={0}>历史累计</NativeSelectOption>
              <NativeSelectOption value={24}>过去 24 小时</NativeSelectOption>
              <NativeSelectOption value={1}>过去 1 小时</NativeSelectOption>
            </NativeSelect>
            <Button variant="outline" size="sm" disabled={loading} onClick={() => setRevision((value) => value + 1)}>
              <RefreshCwIcon className="size-3.5" /> 刷新
            </Button>
            <Button variant="outline" size="sm" disabled={!audit || loading} onClick={exportReport}>
              <DownloadIcon className="size-3.5" /> 导出
            </Button>
          </div>
        </div>
        <CardDescription>总处理量为输入加输出，缓存属于输入的一部分。实付费用需要供应商账单确认。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {loading && <p className="text-muted-foreground text-sm">正在读取用量…</p>}
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
        {audit && total && (
          <>
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              {[
                ["总处理量", tokens(total.total_tokens)],
                ["未命中输入", tokens(total.uncached_input_tokens)],
                ["缓存读取 / 写入", `${tokens(total.cache_read_tokens)} / ${tokens(total.cache_write_tokens)}`],
                ["输出", tokens(total.output_tokens)],
                ["计量记录", tokens(total.rows)],
                ["可识别调用 ID", tokens(total.known_call_ids)],
                ["已记录会话数", total.distinct_sessions === undefined ? "—" : tokens(total.distinct_sessions)],
                ["首条输入 P50", firstInput(total)],
                ["非零输入 P50", total.nonzero_input_rows > 0 ? tokens(total.input_p50) : "—"],
                ["非零输入 P95", total.nonzero_input_rows > 0 ? tokens(total.input_p95) : "—"],
              ].map(([name, value]) => (
                <div key={name} className="rounded-md border p-3">
                  <div className="text-muted-foreground text-xs">{name}</div>
                  <div className="mt-1 break-words font-semibold text-sm tabular-nums">{value}</div>
                </div>
              ))}
            </div>
            <p className="text-muted-foreground text-xs">
              缺调用 ID {tokens(total.missing_call_ids)} · 标记重试 {tokens(total.repeated_attempt_rows)} · 错误记录{" "}
              {tokens(total.error_rows)} · 零用量记录 {tokens(total.zero_usage_rows)}
              <br />
              缺用途 {tokens(total.missing_purpose)} · 缺会话 {tokens(total.missing_session)} · 缺角色{" "}
              {tokens(total.missing_worker)}
            </p>
            {total.invalid_cache_rows > 0 && (
              <p role="alert" className="text-destructive text-sm">
                {tokens(total.invalid_cache_rows)} 条记录的缓存计量异常，请核对原始用量后再估算费用。
              </p>
            )}
            <div className="flex gap-2">
              {(
                [
                  ["by_worker", "按角色"],
                  ["by_purpose", "按用途"],
                  ["by_model", "按模型"],
                ] as const
              ).map(([key, name]) => (
                <Button
                  key={key}
                  size="sm"
                  variant={dimension === key ? "secondary" : "ghost"}
                  onClick={() => setDimension(key)}
                >
                  {name}
                </Button>
              ))}
            </div>
            {total.rows > 0 ? (
              <UsageGroups groups={audit[dimension]} />
            ) : (
              <p className="text-muted-foreground text-sm">所选范围没有计量记录。</p>
            )}
            <p className="text-muted-foreground text-xs">
              统计截至 {new Date(audit.generated_at).toLocaleString()}。调用 ID
              和重试标记只覆盖已记录的数据；零用量记录不能证明上游未扣费。
            </p>
            <p className="text-muted-foreground text-xs">
              首条输入样本 {total.first_turn_input_rows ?? "—"}{" "}
              条，仅包含会话第一条已记录调用的非零输入。会话数不等于实际冷启动次数；时间筛选不会把中途调用重算为首条。
            </p>
          </>
        )}
      </CardContent>
    </Card>
  );
}
