package llm

import (
	"encoding/json"
	"testing"
)

func TestBuildAnthropicSystemCacheBoundary(t *testing.T) {
	sys := []string{"a", "b", "c"}

	cases := []struct {
		name       string
		boundary   int
		wantBlocks int
		wantCached []bool // per-block: has cache_control
	}{
		{"no boundary (0) → single uncached", 0, 1, []bool{false}},
		{"negative → single uncached", -1, 1, []bool{false}},
		{"mid split → cached prefix + dynamic tail", 2, 2, []bool{true, false}},
		{"boundary == len → whole thing cached", 3, 1, []bool{true}},
		{"boundary > len → whole thing cached", 5, 1, []bool{true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocks := buildAnthropicSystem(CompletionRequest{System: sys, DynamicBoundary: c.boundary})
			if len(blocks) != c.wantBlocks {
				t.Fatalf("blocks=%d want %d", len(blocks), c.wantBlocks)
			}
			for i, wantCached := range c.wantCached {
				gotCached := blocks[i].CacheControl != nil
				if gotCached != wantCached {
					t.Fatalf("block %d cached=%v want %v", i, gotCached, wantCached)
				}
			}
		})
	}
}

func TestAnthropicBuildBodyPromptCacheBreakpoints(t *testing.T) {
	p := &anthropicProvider{cfg: Config{Model: "claude-sonnet-4-6"}}
	req := CompletionRequest{
		System:          []string{"static-system", "dynamic-intent"},
		DynamicBoundary: 1,
		Tools: []ToolSchema{
			{Name: "Bash", Description: "run", InputSchema: map[string]any{"type": "object"}},
			{Name: "Read", Description: "read", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []Message{
			UserText("turn 1"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockToolUse, ID: "u1", Name: "Bash", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleUser, Content: []ContentBlock{
				ToolResultText("u1", "ok 1", false),
			}},
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockToolUse, ID: "u2", Name: "Read", Input: json.RawMessage(`{}`)},
			}},
			{Role: RoleUser, Content: []ContentBlock{
				ToolResultText("u2", "ok 2", false),
			}},
		},
	}

	raw, err := p.buildBody(req, true)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var body anthropicWireReq
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Tools: only the last tool gets cache_control.
	if body.Tools[0].CacheControl != nil {
		t.Fatal("first tool must not carry cache_control")
	}
	if body.Tools[1].CacheControl["type"] != "ephemeral" {
		t.Fatalf("last tool missing ephemeral cache_control: %+v", body.Tools[1].CacheControl)
	}

	// System: static prefix cached, dynamic tail uncached.
	if len(body.System) != 2 || body.System[0].CacheControl["type"] != "ephemeral" || body.System[1].CacheControl != nil {
		t.Fatalf("unexpected system cache layout: %+v", body.System)
	}

	// Messages: latest user turn (idx 4) and previous user turn (idx 2) get breakpoints;
	// turn 0 and assistant turns do not. Total breakpoints == 4 (within Anthropic limit).
	if body.Messages[0].Content[0].CacheControl != nil {
		t.Fatal("oldest user turn should not carry cache_control when two newer user turns exist")
	}
	if body.Messages[2].Content[0].CacheControl["type"] != "ephemeral" {
		t.Fatalf("previous user turn missing cache_control: %+v", body.Messages[2].Content[0])
	}
	if body.Messages[4].Content[0].CacheControl["type"] != "ephemeral" {
		t.Fatalf("latest user turn missing cache_control: %+v", body.Messages[4].Content[0])
	}

	totalBP := 0
	for _, tool := range body.Tools {
		if tool.CacheControl != nil {
			totalBP++
		}
	}
	for _, sys := range body.System {
		if sys.CacheControl != nil {
			totalBP++
		}
	}
	for _, msg := range body.Messages {
		for _, blk := range msg.Content {
			if blk.CacheControl != nil {
				totalBP++
			}
		}
	}
	if totalBP != 4 {
		t.Fatalf("total cache breakpoints = %d, want 4", totalBP)
	}

	// DynamicBoundary == 0 disables cache_control across tools, system, and messages.
	req.DynamicBoundary = 0
	rawOff, err := p.buildBody(req, true)
	if err != nil {
		t.Fatalf("buildBody(off): %v", err)
	}
	var bodyOff anthropicWireReq
	if err := json.Unmarshal(rawOff, &bodyOff); err != nil {
		t.Fatalf("unmarshal(off): %v", err)
	}
	if bodyOff.Tools[1].CacheControl != nil || bodyOff.System[0].CacheControl != nil || bodyOff.Messages[4].Content[0].CacheControl != nil {
		t.Fatal("DynamicBoundary=0 should emit no cache_control breakpoints")
	}
}
