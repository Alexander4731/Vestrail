import { firstTurnInputP50Value, usageAuditMarkdown } from "./usage-audit.ts";
import assert from "node:assert/strict";
import test from "node:test";

const usage = Object.freeze({
  rows: 2,
  known_call_ids: 1,
  missing_call_ids: 0,
  repeated_attempt_rows: 1,
  error_rows: 1,
  zero_usage_rows: 0,
  missing_purpose: 0,
  missing_session: 0,
  missing_worker: 0,
  invalid_cache_rows: 0,
  input_tokens: 100_000,
  output_tokens: 10_000,
  cache_read_tokens: 80_000,
  cache_write_tokens: 5_000,
  uncached_input_tokens: 15_000,
  total_tokens: 110_000,
  nonzero_input_rows: 2,
  input_p50: 50_000,
  input_p95: 80_000,
  distinct_sessions: 1,
  first_turn_input_p50: 40_000,
  first_turn_input_rows: 1,
});

const snapshot = Object.freeze({
  task_id: "1",
  source: "llm_usage",
  generated_at: "2026-11-20T10:00:00Z",
  total: usage,
  by_worker: [{ key: "worker", ...usage }],
  by_purpose: [{ key: "chat", ...usage }],
  by_model: [{ key: "model|name\nextra", ...usage }],
});

test("export preserves the exact snapshot without double-counting cache", () => {
  const before = JSON.stringify(snapshot);
  const text = usageAuditMarkdown(snapshot);
  assert.match(text, /总处理量（输入 \+ 输出） \| 110000/);
  assert.match(text, /缓存写入 \| 5000/);
  assert.match(text, /未命中输入 \| 15000/);
  assert.match(text, /标记为重试的记录 \| 1/);
  assert.match(text, /已记录会话数 \| 1/);
  assert.match(text, /首条输入 P50 \| 40000/);
  assert.match(text, /实付费用需要供应商账单确认/);
  assert.equal(JSON.stringify(snapshot), before);
});

test("first-input metrics without samples or from older responses remain unavailable", () => {
  assert.equal(firstTurnInputP50Value({ ...usage, first_turn_input_rows: 0, first_turn_input_p50: 0 }), null);
  const legacy = { ...usage };
  delete legacy.distinct_sessions;
  delete legacy.first_turn_input_p50;
  delete legacy.first_turn_input_rows;
  assert.equal(firstTurnInputP50Value(legacy), null);
  const text = usageAuditMarkdown({ ...snapshot, total: legacy });
  assert.match(text, /首条输入 P50 \| —/);
  assert.match(text, /已记录会话数 \| —/);
});

test("time-filtered exports use the response range instead of a client clock", () => {
  const text = usageAuditMarkdown({ ...snapshot, since: "2026-11-19T10:00:00Z" });
  assert.match(text, /范围起点：2026-11-19T10:00:00Z/);
  assert.match(text, /统计时间（UTC）：2026-11-20T10:00:00Z/);
});

test("model labels cannot inject extra Markdown table cells or lines", () => {
  const text = usageAuditMarkdown(snapshot);
  assert.ok(text.includes("model\\|name extra"));
  assert.ok(!text.includes("model|name\nextra"));
});

test("empty usage exports mark percentiles as unavailable", () => {
  const text = usageAuditMarkdown({
    ...snapshot,
    total: { ...usage, rows: 0, nonzero_input_rows: 0, input_p50: 0, input_p95: 0 },
  });
  assert.match(text, /非零输入 P50 \/ P95 \| —/);
});
