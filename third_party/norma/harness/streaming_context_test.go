package harness

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestStreamingExecutorKeepsOriginalDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var got json.RawMessage
	l := &loop{ctx: context.Background(), toolCtx: ctx, sem: make(chan struct{}, 1), in: QueryInput{Tools: tool.NewRegistry(recordingSpec("probe", false, &got)), PermissionMode: permission.ModeBypass}}
	e := newStreamExec(l)
	cancel()
	l.toolCtx = context.Background() // next turn enters settlement before the queued goroutine starts
	call := &tracked{block: llm.ContentBlock{Type: llm.BlockToolUse, ID: "queued", Name: "probe", Input: json.RawMessage(`{"content":"fixture"}`)}, status: stExecuting}
	e.tools = []*tracked{call}
	e.run(call)
	if got != nil || !call.result.IsError {
		t.Fatal("queued tool inherited the live settlement context")
	}
}

func TestLateToolResultDoesNotReplaceInterruptedTranscript(t *testing.T) {
	var got json.RawMessage
	l := &loop{ctx: context.Background(), sem: make(chan struct{}, 1), in: QueryInput{Tools: tool.NewRegistry(recordingSpec("probe", false, &got)), PermissionMode: permission.ModeBypass}}
	e := newStreamExec(l)
	call := &tracked{block: llm.ContentBlock{Type: llm.BlockToolUse, ID: "late", Name: "probe", Input: json.RawMessage(`{"content":"fixture"}`)}, status: stYielded, result: llm.ToolResultText("late", "tool execution interrupted", true)}
	e.tools = []*tracked{call}
	e.run(call)
	if got == nil {
		t.Fatal("fixture tool did not finish")
	}
	if call.status != stYielded || !call.result.IsError || call.result.Content[0].Text != "tool execution interrupted" {
		t.Fatal("late result replaced sealed transcript")
	}
}
