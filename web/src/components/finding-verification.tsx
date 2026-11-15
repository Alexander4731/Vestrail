"use client";

import * as React from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { Finding, FindingVerification, VerificationAttempt } from "@/lib/types";

const labels: Record<FindingVerification["state"], string> = {
  legacy_unverified: "历史未验证",
  candidate: "候选发现",
  running: "验证中",
  verified: "机器已验证",
  not_reproduced: "未复现",
  inconclusive: "无法判定",
};

export function VerificationBadge({ value }: { value?: FindingVerification }) {
  return <Badge variant="outline">{value?.stale ? "验证已过期" : labels[value?.state ?? "legacy_unverified"]}</Badge>;
}

export function FindingVerificationPanel({ finding, onChanged }: { finding: Finding; onChanged: () => void }) {
  const [items, setItems] = React.useState<VerificationAttempt[]>([]);
  const [error, setError] = React.useState("");
  const [loaded, setLoaded] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [more, setMore] = React.useState(false);
  const seq = React.useRef(0);
  const pending = React.useRef<{ key: string; version: number } | null>(null);
  const load = React.useCallback(
    async (before?: string) => {
      const request = ++seq.current;
      try {
        const result = await api.findingVerifications(finding.id, before);
        if (request !== seq.current) return;
        setItems((old) => (before ? [...old, ...result.attempts] : result.attempts));
        setMore(result.attempts.length === 100);
        setError("");
        setLoaded(true);
      } catch (e) {
        if (request === seq.current) setError((e as Error).message);
      }
    },
    [finding.id],
  );
  React.useEffect(() => {
    void load();
    return () => {
      seq.current++;
    };
  }, [load]);
  React.useEffect(() => {
    if (!items.some((item) => item.state === "running")) return;
    const timer = setTimeout(() => {
      void load().then(onChanged);
    }, 2000);
    return () => clearTimeout(timer);
  }, [items, load, onChanged]);
  const run = async () => {
    setBusy(true);
    setError("");
    const version = finding.evidence_version ?? 0;
    if (!pending.current || pending.current.version !== version)
      pending.current = { key: crypto.randomUUID(), version };
    try {
      await api.verifyFinding(finding.id, version, pending.current.key);
      pending.current = null;
      await load();
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          证据验证 <VerificationBadge value={finding.verification} />
        </CardTitle>
        <CardDescription>
          完整性检查核对已保存的流量证据，不发送请求。检查通过仅说明材料完整，漏洞结论仍需适用的验证判据。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {!finding.inherited && (
          <Button
            variant="outline"
            disabled={busy || finding.verification?.state === "running"}
            onClick={() => void run()}
          >
            {busy ? "检查中…" : "检查证据完整性"}
          </Button>
        )}
        {error && (
          <div role="alert" className="text-destructive text-sm">
            {error}{" "}
            <Button size="sm" variant="ghost" onClick={() => void load()}>
              重新加载
            </Button>
          </div>
        )}
        {!loaded && !error && <p className="text-muted-foreground text-sm">加载验证记录…</p>}
        {loaded && !items.length && <p className="text-muted-foreground text-sm">尚无验证记录。</p>}
        {items.map((item) => (
          <div key={item.id} className="rounded-lg border p-3 text-sm">
            <p>
              #{item.id} · 证据版本 {item.evidence_version} ·{" "}
              {
                { evidence_bound: "证据来源已核对", semantic_verified: "限定判据已验证", none: "未获得验证结论" }[
                  item.assurance
                ]
              }
            </p>
            <p className="mt-1 whitespace-pre-wrap">
              {item.reason || (item.state === "running" ? "验证进行中" : "无说明")}
            </p>
            <p className="mt-1 text-muted-foreground text-xs">
              {item.verifier} v{item.verifier_version} · {item.actor} ·{" "}
              {new Date(item.created_at).toLocaleString("zh-CN")}
            </p>
          </div>
        ))}
        {more && (
          <Button variant="ghost" onClick={() => void load(items.at(-1)?.id)}>
            加载更早记录
          </Button>
        )}
      </CardContent>
    </Card>
  );
}
