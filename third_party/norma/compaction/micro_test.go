package compaction

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func manyReadPairs(n int) []llm.Message {
	var msgs []llm.Message
	for i := range n {
		msgs = append(msgs, toolPair("Read", "r"+strconv.Itoa(i), strings.Repeat("y", 200))...)
	}
	return msgs
}

func countCleared(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && strings.Contains(resultText(b), "cleared") {
				n++
			}
		}
	}
	return n
}

func TestMicroExactlyKeepRecentNoChange(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	msgs := manyReadPairs(microKeepRecent) // exactly the kept window
	out, changed := c.microCompact(msgs)
	if changed || countCleared(out) != 0 {
		t.Fatalf("with exactly %d results nothing should clear (changed=%v cleared=%d)", microKeepRecent, changed, countCleared(out))
	}
}

func TestMicroClearsOldestBeyondWindow(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	for _, extra := range []int{1, 3, 7} {
		msgs := manyReadPairs(microKeepRecent + extra)
		out, changed := c.microCompact(msgs)
		if !changed {
			t.Fatalf("extra=%d: expected a change", extra)
		}
		if got := countCleared(out); got != extra {
			t.Fatalf("extra=%d: cleared=%d, want %d (all but the last %d)", extra, got, extra, microKeepRecent)
		}
		// The most recent microKeepRecent results must be intact.
		if strings.Contains(resultText(out[len(out)-1].Content[0]), "cleared") {
			t.Fatalf("extra=%d: most-recent result was wrongly cleared", extra)
		}
	}
}

func TestMicroNoCompactableNoChange(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	var msgs []llm.Message
	for i := range 10 {
		msgs = append(msgs, toolPair("Agent", "a"+strconv.Itoa(i), strings.Repeat("y", 500))...)
	}
	if _, changed := c.microCompact(msgs); changed {
		t.Fatal("no compactable tools → no change")
	}
}

func TestMicroIdempotent(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	msgs := manyReadPairs(microKeepRecent + 3)
	out1, changed1 := c.microCompact(msgs)
	if !changed1 {
		t.Fatal("first pass should change")
	}
	out2, changed2 := c.microCompact(out1)
	if changed2 {
		t.Fatal("second pass should be a no-op (already cleared)")
	}
	if countCleared(out1) != countCleared(out2) {
		t.Fatal("idempotence violated")
	}
}

func TestMicroPreservesSiblingBlocks(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	// Target result (oldest) lives in a message alongside a text block.
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "t0", Name: "Read"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock("KEEP ME"),
			llm.ToolResultText("t0", strings.Repeat("z", 500), false),
		}},
	}
	msgs = append(msgs, manyReadPairs(microKeepRecent)...) // push t0 out of the keep window
	out, changed := c.microCompact(msgs)
	if !changed {
		t.Fatal("expected a change")
	}
	sib := out[1].Content
	if len(sib) != 2 || sib[0].Text != "KEEP ME" {
		t.Fatalf("sibling text block not preserved: %+v", sib)
	}
	if !strings.Contains(resultText(sib[1]), "cleared") {
		t.Fatal("target result should be cleared")
	}
}

func TestMicroKeepsRecentToolResult(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	msgs := manyReadPairs(microKeepRecent + 1)
	out, _ := c.microCompact(msgs)
	// Only the oldest (r0) result is cleared; r1..r5 stay.
	if !strings.Contains(resultText(out[1].Content[0]), "cleared") {
		t.Fatal("oldest (r0) should be cleared")
	}
	for i := 1; i <= microKeepRecent; i++ {
		resultMsg := out[2*i+1] // each pair is [tool_use, tool_result]
		if strings.Contains(resultText(resultMsg.Content[0]), "cleared") {
			t.Fatalf("recent result r%d should be intact", i)
		}
	}
}

func TestMicroMultipleResultsInOneMessage(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	// Parallel tool_use then one user message carrying both results.
	msgs := []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "a", Name: "Read"},
			{Type: llm.BlockToolUse, ID: "b", Name: "Grep"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultText("a", strings.Repeat("y", 300), false),
			llm.ToolResultText("b", strings.Repeat("y", 300), false),
		}},
	}
	msgs = append(msgs, manyReadPairs(microKeepRecent)...) // push a,b out of the window
	out, changed := c.microCompact(msgs)
	if !changed {
		t.Fatal("expected a change")
	}
	both := out[1].Content
	if !strings.Contains(resultText(both[0]), "cleared") || !strings.Contains(resultText(both[1]), "cleared") {
		t.Fatalf("both old results in the shared message should be cleared: %+v", both)
	}
}

// A compactable tool_use whose result is still within the recent window must not
// be cleared even if OTHER (older) compactable results exist.
func TestMicroRecentWindowProtectsByToolNotMessage(t *testing.T) {
	c := New(Config{ContextWindow: 200000}, nil)
	msgs := manyReadPairs(microKeepRecent + 2)
	out, _ := c.microCompact(msgs)
	if got := countCleared(out); got != 2 {
		t.Fatalf("cleared=%d, want 2 (only the two oldest)", got)
	}
}

func TestMicroExtraCompactableAndPersistedPathPreserved(t *testing.T) {
	c := New(Config{
		ContextWindow:    200000,
		MicroKeepRecent:  2,
		ExtraCompactable: []string{"traffic_get", "list_assets"},
	}, nil)
	var msgs []llm.Message
	msgs = append(msgs, toolPair("traffic_get", "t0", strings.Repeat("a", 500)+"\n... <persisted-output>[Output too large: full 50000 bytes / 100 lines.Full output saved to  /work/cmd-output/out.txt </persisted-output>")...)
	msgs = append(msgs, toolPair("list_assets", "t1", strings.Repeat("b", 500))...)
	msgs = append(msgs, toolPair("traffic_get", "t2", "recent-1")...)
	msgs = append(msgs, toolPair("list_assets", "t3", "recent-2")...)

	out, changed := c.microCompact(msgs)
	if !changed {
		t.Fatal("expected extra compactable tools to be cleared")
	}
	if got := countCleared(out); got != 2 {
		t.Fatalf("cleared=%d, want 2", got)
	}
	r0 := resultText(out[1].Content[0])
	if !strings.Contains(r0, "/work/cmd-output/out.txt") {
		t.Fatalf("expected persisted file path to survive microCompact, got: %q", r0)
	}
	// Idempotent on second sweep.
	if _, changed2 := c.microCompact(out); changed2 {
		t.Fatal("second microCompact pass should be idempotent for enriched placeholders")
	}
}

func TestMicroBatchHysteresisPreservesPromptCache(t *testing.T) {
	c := New(Config{
		ContextWindow:    200000,
		SoftWindow:       60000, // eff=52000, microThreshold=31200, autoThreshold=39000
		MicroKeepRecent:  2,
		MicroBatchGrowth: 15000,
	}, nil)
	// First sweep fires once tokens exceed microThreshold (31200).
	msgs := manyReadPairs(5)
	out1 := c.Pre(t.Context(), msgs, 33000)
	if got := countCleared(out1); got != 3 {
		t.Fatalf("first sweep cleared=%d, want 3", got)
	}
	// Next turn adds one more tool call; even if provider reports 32000 (> microThreshold),
	// growth since lastMicroTokens is below MicroBatchGrowth (15000) and below autoThreshold,
	// so Pre MUST NOT mutate older messages (keeps prompt cache intact).
	msgs2 := append(out1, toolPair("Read", "r_new", strings.Repeat("z", 400))...)
	out2 := c.Pre(t.Context(), msgs2, 32000)
	if got := countCleared(out2); got != 3 {
		t.Fatalf("hysteresis should skip intermediate sweep to protect prompt cache; cleared=%d want 3", got)
	}
	// Reaching AutoCompact's threshold overrides the growth delay.
	out3 := c.Pre(t.Context(), msgs2, 41000)
	if got := countCleared(out3); got != 4 {
		t.Fatalf("expected batch sweep after growth threshold; cleared=%d want 4", got)
	}
}

func offsetScaleHistory() []llm.Message {
	msgs := toolPair("Read", "large-old", strings.Repeat("x", 15000))
	msgs = append(msgs, manyReadPairs(4)...)
	// Non-tool context plus system/tools overhead makes local estimates differ
	// substantially from the provider's reported input size.
	return append(msgs, llm.UserText(strings.Repeat("context", 7300)))
}

func TestMicroBatchHysteresisWithRealTokenScaleOffset(t *testing.T) {
	c := New(Config{ContextWindow: 200000, SoftWindow: 90000, MicroKeepRecent: 2, MicroBatchGrowth: 20000}, nil)
	first := c.Pre(t.Context(), offsetScaleHistory(), 55000)
	if countCleared(first) != 3 {
		t.Fatalf("first sweep did not clear old results: %d", countCleared(first))
	}
	if c.lastMicroTokens < 49000 || c.lastMicroTokens > 51500 {
		t.Fatalf("provider scale was lost after clearing about 5k tokens: %d", c.lastMicroTokens)
	}
	next := append(first, toolPair("Read", "new-result", "keep this result")...)
	before := append([]llm.Message(nil), next...)
	out := c.Pre(t.Context(), next, 58000)
	if !reflect.DeepEqual(out, before) || !reflect.DeepEqual(next, before) {
		t.Fatal("a small increment rewrote the prompt despite the growth delay")
	}
}

func TestMicroRealTokenOffsetDoesNotSuppressAutoCompact(t *testing.T) {
	calls := 0
	c := New(Config{ContextWindow: 200000, SoftWindow: 90000, MicroKeepRecent: 2, MicroBatchGrowth: 20000},
		func(_ context.Context, _ []llm.Message) (string, error) {
			calls++
			return "complete summary", nil
		})
	out := c.Pre(t.Context(), offsetScaleHistory(), 80000)
	if calls != 1 || boundaryCount(out) != 1 {
		t.Fatalf("dropping provider overhead hid an actual compaction threshold: calls=%d", calls)
	}
}
