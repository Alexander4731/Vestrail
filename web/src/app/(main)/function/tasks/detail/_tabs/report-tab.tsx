"use client";

import * as React from "react";

import { CheckIcon, CopyIcon, FileTextIcon } from "lucide-react";
import { toast } from "sonner";

import { CoverageMatrix } from "@/components/coverage-matrix";
import { Markdown } from "@/components/markdown";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import { copyText } from "@/lib/utils";

export function ReportTab({ taskId }: { taskId: string }) {
  const [report, setReport] = React.useState<string>("");
  const [loading, setLoading] = React.useState(true);
  const [copied, setCopied] = React.useState(false);
  const [error, setError] = React.useState<string>("");
  const requestSeq = React.useRef(0);

  const load = React.useCallback(async () => {
    const request = ++requestSeq.current;
    setLoading(true);
    setError("");
    setReport("");
    try {
      const text = await api.report(taskId);
      if (request === requestSeq.current) setReport(text);
    } catch (cause) {
      if (request === requestSeq.current) setError(cause instanceof Error ? cause.message : "报告读取失败，请重试");
    } finally {
      if (request === requestSeq.current) setLoading(false);
    }
  }, [taskId]);
  React.useEffect(() => {
    void load();
    return () => {
      requestSeq.current++;
    };
  }, [load]);

  async function copy() {
    if (!report) return;
    const ok = await copyText(report);
    if (ok) {
      setCopied(true);
      toast.success("已复制 Markdown");
      setTimeout(() => setCopied(false), 1500);
    } else {
      toast.error("复制失败，请手动选择文本复制");
    }
  }

  let content: React.ReactNode;
  if (loading) {
    content = (
      <div className="flex flex-col items-center justify-center gap-2 rounded-md border border-dashed py-16 text-muted-foreground text-sm">
        <FileTextIcon className="size-8 opacity-40" />
        加载中…
      </div>
    );
  } else if (error) {
    content = (
      <div className="flex flex-col items-center gap-3 rounded-md border border-destructive/30 p-8" role="alert">
        <p className="text-destructive text-sm">{error}</p>
        <Button size="sm" variant="outline" onClick={() => void load()}>
          重试
        </Button>
      </div>
    );
  } else if (report) {
    content = (
      <div className="max-h-[60vh] overflow-auto rounded-md border bg-muted/20 p-4">
        <Markdown text={report} />
      </div>
    );
  } else {
    content = (
      <div className="flex flex-col items-center justify-center gap-2 rounded-md border border-dashed py-16 text-muted-foreground text-sm">
        <FileTextIcon className="size-8 opacity-40" />
        暂无报告
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <CoverageMatrix key={taskId} taskId={taskId} />
      <Card>
        <CardHeader className="flex-row items-center justify-between">
          <CardTitle className="flex items-center gap-2 text-sm">
            <FileTextIcon className="size-4" /> 渗透测试报告（Markdown）
          </CardTitle>
          <div className="flex gap-2">
            {report && (
              <Button size="sm" variant="outline" onClick={copy}>
                {copied ? <CheckIcon /> : <CopyIcon />} 复制
              </Button>
            )}
          </div>
        </CardHeader>
        <CardContent>{content}</CardContent>
      </Card>
    </div>
  );
}
