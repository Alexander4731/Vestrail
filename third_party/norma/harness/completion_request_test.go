package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestHostCompletionRecordsToolResultWithoutAnotherModelTurn(t *testing.T) {
	ctx := WithCompletionRequest(t.Context())
	registry := tool.NewRegistry()
	registry.Add(tool.Build(tool.Spec{Name: "finish", Schema: map[string]any{"type": "object"}, Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		RequestCompletion(ctx, "verified delivery")
		return tool.Text("receipt stored"), nil
	}}))
	calls := 0
	deps := QueryDeps{CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockToolUse, ID: "finish-1", Name: "finish", Input: json.RawMessage(`{}`)}}}, "tool_use", llm.Usage{InputTokens: 10, OutputTokens: 1}, nil
	}}
	var terminal *Terminal
	for event, err := range Query(ctx, QueryInput{Messages: []llm.Message{llm.UserText("fixture")}, Tools: registry, PermissionMode: permission.ModeBypass, NonStreaming: true, MaxTurns: 1}, deps) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Terminal != nil {
			terminal = event.Terminal
		}
	}
	if calls != 1 || terminal == nil || terminal.Reason != ReasonCompleted || terminal.Text != "verified delivery" {
		t.Fatalf("calls=%d terminal=%+v", calls, terminal)
	}
}
