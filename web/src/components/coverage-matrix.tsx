"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { CoverageCheck, CoverageCheckSpec, CoverageLedger } from "@/lib/types";

const states: Record<CoverageCheck["state"], string> = {
  not_run: "未运行",
  running: "运行中",
  observed_no_issue: "本次未观察到问题",
  finding: "发现问题",
  blocked: "受阻",
  not_applicable: "人工排除",
  needs_review: "待审阅",
};
const emptyCheck = (): CoverageCheckSpec => ({
  target_ref: "",
  principal_ref: "anonymous",
  category: "",
  test_id: "",
  precondition_version: 1,
  prerequisites: "",
});

function CheckResultForm({ taskId, check, onSaved }: { taskId: string; check: CoverageCheck; onSaved: () => void }) {
  const [outcome, setOutcome] = React.useState("blocked");
  const [reason, setReason] = React.useState("");
  const [evidenceId, setEvidenceId] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const request = React.useRef<{ data: string; key: string } | null>(null);
  async function save() {
    const data = JSON.stringify({ outcome, reason, evidenceId });
    if (request.current?.data !== data) request.current = { data, key: crypto.randomUUID() };
    setBusy(true);
    setError("");
    try {
      await api.recordCoverageCheck(taskId, check.id, {
        request_key: request.current.key,
        outcome,
        reason,
        ...(evidenceId.trim() ? { evidence_node_id: evidenceId.trim() } : {}),
      });
      onSaved();
      setReason("");
      request.current = null;
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <details className="mt-2">
      <summary className="cursor-pointer">记录观察或缺口</summary>
      <div className="mt-2 flex flex-col gap-2">
        <select
          aria-label="检查结果"
          className="rounded-md border bg-background p-2"
          value={outcome}
          onChange={(e) => setOutcome(e.target.value)}
        >
          <option value="blocked">受阻 / 未完成</option>
          <option value="reported_no_issue">自报未发现问题</option>
          <option value="finding">自报发现问题</option>
          <option value="needs_review">需要审阅</option>
          <option value="not_applicable">人工确认不适用</option>
        </select>
        <Textarea
          value={reason}
          aria-label="观察或排除理由"
          placeholder="记录前提状态、实际观察及理由"
          maxLength={2000}
          onChange={(e) => setReason(e.target.value)}
        />
        <Input
          value={evidenceId}
          aria-label="证据节点 ID"
          placeholder="本任务事实或发现节点 ID（可选）"
          onChange={(e) => setEvidenceId(e.target.value)}
        />
        <Button variant="outline" disabled={busy || !reason.trim()} onClick={() => void save()}>
          保存记录
        </Button>
        {error && (
          <p role="alert" className="text-destructive">
            {error}
          </p>
        )}
      </div>
    </details>
  );
}

function PlanEditor({ taskId, previous, onSaved }: { taskId: string; previous: string; onSaved: () => void }) {
  const [checks, setChecks] = React.useState<(CoverageCheckSpec & { localId: number })[]>([
    { ...emptyCheck(), localId: 0 },
  ]);
  const nextId = React.useRef(1);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const request = React.useRef<{ data: string; key: string } | null>(null);
  function update(index: number, key: keyof CoverageCheckSpec, value: string) {
    setChecks((old) => old.map((row, i) => (i === index ? { ...row, [key]: value } : row)));
  }
  async function save() {
    const data = JSON.stringify({ previous, checks });
    if (request.current?.data !== data) request.current = { data, key: crypto.randomUUID() };
    setBusy(true);
    setError("");
    try {
      await api.createCoveragePlan(
        taskId,
        previous,
        request.current.key,
        checks.map(({ localId: _, ...spec }) => spec),
      );
      onSaved();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="flex flex-col gap-3 rounded-lg border p-3">
      <p className="text-sm">填写本次适用的完整检查集合。保存后产生新修订，旧结果保留；本表不会自动执行测试。</p>
      {checks.map((check, index) => (
        <div key={check.localId} className="grid gap-2 rounded-md border p-3 md:grid-cols-2">
          {(
            [
              ["target_ref", "目标（资产 ID、资源路径等）"],
              ["principal_ref", "测试身份名称 / anonymous / unknown"],
              ["category", "测试类别"],
              ["test_id", "检查项名称"],
            ] as const
          ).map(([key, label]) => (
            <Input
              key={key}
              aria-label={`${index + 1} ${label}`}
              placeholder={label}
              value={check[key]}
              onChange={(e) => update(index, key, e.target.value)}
            />
          ))}
          <Textarea
            aria-label={`${index + 1} 前提条件`}
            placeholder="前提条件（请勿填写密码或令牌）"
            value={check.prerequisites}
            maxLength={1000}
            onChange={(e) => update(index, "prerequisites", e.target.value)}
          />
          <Button
            variant="ghost"
            disabled={checks.length <= 1}
            onClick={() => setChecks((old) => old.filter((_, i) => i !== index))}
          >
            移除该项
          </Button>
        </div>
      ))}
      <div className="flex flex-wrap gap-2">
        <Button
          variant="outline"
          disabled={checks.length >= 1000}
          onClick={() => {
            const localId = nextId.current++;
            setChecks((old) => [...old, { ...emptyCheck(), localId }]);
          }}
        >
          添加检查项
        </Button>
        <Button
          disabled={busy || checks.some((c) => !c.target_ref.trim() || !c.category.trim() || !c.test_id.trim())}
          onClick={() => void save()}
        >
          保存计划修订
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
    </div>
  );
}

export function CoverageMatrix({ taskId }: { taskId: string }) {
  const [ledger, setLedger] = React.useState<CoverageLedger | null>(null);
  const [error, setError] = React.useState("");
  const [editing, setEditing] = React.useState(false);
  const [historyRevision, setHistoryRevision] = React.useState("");
  const seq = React.useRef(0);
  const load = React.useCallback(
    async (planId?: string, after?: string) => {
      const request = ++seq.current;
      try {
        const result = await api.coverageLedger(taskId, planId, after);
        if (request !== seq.current) return;
        setLedger((old) =>
          after && old && old.plan?.id === result.plan?.id
            ? { ...result, items: [...old.items, ...result.items] }
            : result,
        );
        setError("");
      } catch (e) {
        if (request === seq.current) setError((e as Error).message);
      }
    },
    [taskId],
  );
  React.useEffect(() => {
    setLedger(null);
    setEditing(false);
    void load();
    return () => {
      seq.current++;
    };
  }, [load]);
  return (
    <Card>
      <CardHeader>
        <CardTitle>测试检查账本</CardTitle>
        <CardDescription>
          覆盖率只针对已声明的适用检查集合。自报结果与机器观察分别统计，尚无受信任观察的检查保留缺口。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}{" "}
            <Button variant="ghost" onClick={() => void load()}>
              重试
            </Button>
          </p>
        )}
        {!ledger && !error && <p>加载检查计划…</p>}
        {ledger?.availability === "unavailable" && (
          <p className="text-muted-foreground text-sm">尚未声明测试计划，覆盖结果不可用。</p>
        )}
        {ledger?.plan && (
          <>
            <p className="text-sm">
              计划 #{ledger.plan.id} · 修订 {ledger.plan.revision} · 总数 {ledger.total} · 人工排除 {ledger.excluded} ·
              适用 {ledger.applicable} · 机器完成 {ledger.completed} · 自报 {ledger.reported} · 缺口 {ledger.gaps}
            </p>
            {ledger.scope_stale && (
              <p role="alert" className="text-destructive text-sm">
                任务范围已变化，请声明新的计划修订。旧计划不代表当前范围。
              </p>
            )}
            {ledger.items.map((check) => (
              <div key={check.id} className="rounded-lg border p-3 text-sm">
                <p className="font-medium">
                  {check.target_ref} · {check.principal_ref} · {check.category} / {check.test_id}
                </p>
                <p>
                  {states[check.state]}
                  {check.evidence_stale ? " · 证据已变化" : ""}
                </p>
                {check.prerequisites && <p className="text-muted-foreground">前提：{check.prerequisites}</p>}
                {check.reason && <p className="mt-1 whitespace-pre-wrap">{check.reason}</p>}
                {!ledger.scope_stale && <CheckResultForm taskId={taskId} check={check} onSaved={() => void load()} />}
              </div>
            ))}
            {ledger.next_after && (
              <Button variant="outline" onClick={() => void load(ledger.plan?.id, ledger.next_after)}>
                加载更多检查
              </Button>
            )}
          </>
        )}
        {ledger && !editing && (
          <Button
            variant="outline"
            onClick={() => {
              setHistoryRevision(ledger.plan?.id ?? "0");
              setEditing(true);
            }}
          >
            声明新的检查计划
          </Button>
        )}
        {editing && (
          <PlanEditor
            taskId={taskId}
            previous={historyRevision}
            onSaved={() => {
              setEditing(false);
              void load();
            }}
          />
        )}
      </CardContent>
    </Card>
  );
}
