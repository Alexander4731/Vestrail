type TokenUsage = { input: number; output: number; cacheRead: number };

// Provider-normalized input already includes cache reads. Split display series
// without changing the ledger totals or adding the same tokens twice.
export function splitTokenUsage(usage: TokenUsage) {
  const input = Math.max(0, usage.input);
  const output = Math.max(0, usage.output);
  const cacheRead = Math.min(input, Math.max(0, usage.cacheRead));
  return {
    inputWithoutCacheRead: input - cacheRead,
    cacheRead,
    output,
    total: input + output,
    cacheHitPercent: input > 0 ? (cacheRead / input) * 100 : 0,
  };
}
