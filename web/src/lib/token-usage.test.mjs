import { splitTokenUsage } from "./token-usage.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("a 100k input with 80k cached and 10k output stacks to 110k", () => {
  const parts = splitTokenUsage({ input: 100_000, cacheRead: 80_000, output: 10_000 });
  assert.equal(parts.inputWithoutCacheRead, 20_000);
  assert.equal(parts.cacheRead, 80_000);
  assert.equal(parts.output, 10_000);
  assert.equal(parts.total, 110_000);
  assert.equal(parts.inputWithoutCacheRead + parts.cacheRead + parts.output, parts.total);
  assert.equal(parts.cacheHitPercent, 80);
});

test("task 1 historical totals remain unchanged when split for display", () => {
  const ledger = Object.freeze({ input: 2_152_234_706, cacheRead: 1_990_814_310, output: 20_486_377 });
  const parts = splitTokenUsage(ledger);
  assert.equal(parts.total, 2_172_721_083);
  assert.equal(parts.inputWithoutCacheRead, 161_420_396);
  assert.equal(parts.inputWithoutCacheRead + parts.cacheRead + parts.output, parts.total);
  assert.deepEqual(ledger, { input: 2_152_234_706, cacheRead: 1_990_814_310, output: 20_486_377 });
});

test("empty usage renders finite zero totals and cache percentage", () => {
  assert.deepEqual(splitTokenUsage({ input: 0, output: 0, cacheRead: 0 }), {
    inputWithoutCacheRead: 0,
    cacheRead: 0,
    output: 0,
    total: 0,
    cacheHitPercent: 0,
  });
});

test("inconsistent historical cache subsets cannot produce negative chart segments", () => {
  const ledger = Object.freeze({ input: 100, cacheRead: 120, output: 10 });
  const parts = splitTokenUsage(ledger);
  assert.equal(parts.inputWithoutCacheRead, 0);
  assert.equal(parts.cacheRead, 100);
  assert.equal(parts.cacheHitPercent, 100);
  assert.equal(parts.inputWithoutCacheRead + parts.cacheRead + parts.output, 110);
  assert.equal(ledger.cacheRead, 120);
});

test("profile buckets preserve the same total after aggregation", () => {
  const buckets = [
    { input: 100_000, output: 10_000, cacheRead: 80_000 },
    { input: 50_000, output: 5_000, cacheRead: 0 },
  ];
  const combined = buckets.reduce(
    (total, bucket) => ({
      input: total.input + bucket.input,
      output: total.output + bucket.output,
      cacheRead: total.cacheRead + bucket.cacheRead,
    }),
    { input: 0, output: 0, cacheRead: 0 },
  );
  const parts = splitTokenUsage(combined);
  assert.equal(parts.inputWithoutCacheRead + parts.cacheRead + parts.output, 165_000);
  assert.equal(
    parts.total,
    buckets.reduce((total, bucket) => total + splitTokenUsage(bucket).total, 0),
  );
});
