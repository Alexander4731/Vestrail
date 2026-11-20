package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIStripsPriorTurnReasoning(t *testing.T) {
	old := ContentBlock{Type: BlockToolUse, ID: "old", Name: "Read", Input: json.RawMessage(`{}`), ThoughtSignature: "opaque-signature"}
	current := ContentBlock{Type: BlockToolUse, ID: "current", Name: "Read", Input: json.RawMessage(`{}`)}
	msgs := []Message{
		UserText("old question"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "old scratchpad"}, old}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("old", "old evidence", false)}},
		{Role: RoleAssistant, Content: []ContentBlock{TextBlock("old answer")}},
		UserText("new question"),
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "current scratchpad"}, current}},
		{Role: RoleUser, Content: []ContentBlock{ToolResultText("current", "current evidence", false)}},
	}
	p := &openaiProvider{cfg: Config{Model: "deepseek", ThinkingType: "enabled"}}
	body, err := p.buildBody(CompletionRequest{Messages: msgs}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "old scratchpad") || !strings.Contains(string(body), "current scratchpad") || !strings.Contains(string(body), "opaque-signature") {
		t.Fatalf("history boundary or tool signature was corrupted: %s", body)
	}
	var parsed struct {
		Messages []oaMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	for _, m := range parsed.Messages {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "old" && m.ReasoningContent != "" {
			t.Fatalf("historical reasoning was reintroduced: %+v", m)
		}
	}
}

func TestOpenAIChainPreservesPriorReasoningPolicy(t *testing.T) {
	p := &openaiProvider{cfg: Config{Model: "deepseek", ThinkingType: "enabled"}}
	msgs := []Message{UserText("first"), {Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Thinking: "retain-for-prefix"}, TextBlock("result")}}, UserText("next chain")}
	body, e := p.buildBody(CompletionRequest{Messages: msgs, PreserveReasoningHistory: true}, false)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(body), "retain-for-prefix") {
		t.Fatal("chain prefix rewritten")
	}
}
