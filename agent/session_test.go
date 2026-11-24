package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func TestSessionRejectsIncompleteToolTurnBeforeExecution(t *testing.T) {
	for _, failure := range []string{"stream EOF", "silent EOF", "invalid arguments", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			probe := tool.Build(tool.Spec{Name: "Probe", Schema: map[string]any{"type": "object"},
				Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					calls++
					return tool.Text("executed"), nil
				},
			})
			provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				input, stop := `{}`, "tool_use"
				if failure == "invalid arguments" {
					input = `{"path":`
				}
				if failure == "silent EOF" {
					stop = ""
				}
				for _, ev := range []llm.StreamEvent{
					{Type: llm.SETextDelta, Text: "working"},
					{Type: llm.SEToolUseStart, ToolID: "first", ToolName: "Probe"},
					{Type: llm.SEToolInputJSON, Text: `{}`},
					{Type: llm.SEToolUseStart, ToolID: "second", ToolName: "Probe"},
					{Type: llm.SEToolInputJSON, Text: input},
					{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: 7}},
				} {
					if !yield(ev, nil) {
						return
					}
				}
				switch failure {
				case "stream EOF":
					yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
				case "cancelled":
					cancel()
				}
			}}
			store := transcript.NewStore(t.TempDir())
			var activity []db.Activity
			_, _, err := captureRun(ctx, agentcore.Options{Provider: provider, Tools: []tool.CoreTool{probe},
				PermissionMode: permission.ModeBypass, MaxTurns: 1, Transcript: store, SessionID: "failed-turn"}, "go", func(a db.Activity) {
				activity = append(activity, a)
			})
			if err == nil || ((failure == "stream EOF" || failure == "silent EOF") && !errors.Is(err, io.ErrUnexpectedEOF)) {
				t.Fatalf("wrong terminal error: %v", err)
			}
			if calls != 0 {
				t.Fatalf("incomplete response executed %d tools", calls)
			}
			for _, a := range activity {
				if a.Kind == "tool_use" || a.Kind == "tool_result" {
					t.Fatalf("incomplete call was dispatched: %+v", a)
				}
			}
			assertCapturedResultUsage(t, activity, 0, 7, 0, 0)
			records, err := store.Load(store.MainPath("failed-turn"))
			if err != nil {
				t.Fatal(err)
			}
			messages, _ := transcript.Messages(records)
			if len(messages) != 1 || messages[0].Role != llm.RoleUser {
				t.Fatalf("incomplete tools entered resumable history: %+v", messages)
			}
		})
	}
}

func TestSessionCompletedToolTurnPersistsExactResults(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "streaming", true: "nonstreaming"}[nonStreaming], func(t *testing.T) {
			streamEnded, calls, textSeen := false, 0, false
			provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "working"}, nil) {
					return
				}
				if !nonStreaming && !textSeen {
					t.Error("initial text stopped streaming")
				}
				for _, ev := range []llm.StreamEvent{
					{Type: llm.SEToolUseStart, ToolID: "probe", ToolName: "Probe"},
					{Type: llm.SEToolInputJSON, Text: `{}`},
					{Type: llm.SEMessageDelta, StopReason: "tool_use"},
					{Type: llm.SEMessageStop},
				} {
					if !yield(ev, nil) {
						return
					}
				}
				streamEnded = true
			}}
			probe := tool.Build(tool.Spec{Name: "Probe", Schema: map[string]any{"type": "object"},
				Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
					if !streamEnded {
						t.Error("tool started before the model response completed")
					}
					calls++
					return tool.Text("actual output"), nil
				},
			})
			store := transcript.NewStore(t.TempDir())
			s := newAgentSession(agentcore.Options{Provider: provider, Tools: []tool.CoreTool{probe},
				PermissionMode: permission.ModeBypass, NonStreaming: nonStreaming, MaxTurns: 1, Transcript: store, SessionID: "complete-turn"})
			defer s.Close()
			for ev, err := range s.Prompt(t.Context(), "go") {
				if err != nil {
					t.Fatal(err)
				}
				if ev.Kind == harness.KindText {
					textSeen = true
				}
			}
			if calls != 1 {
				t.Fatalf("tool executed %d times", calls)
			}
			records, err := store.Load(store.MainPath("complete-turn"))
			if err != nil {
				t.Fatal(err)
			}
			messages, _ := transcript.Messages(records)
			if len(messages) != 3 || len(messages[1].ToolUses()) != 1 || len(messages[2].Content) != 1 || messages[2].Content[0].ToolUseID != "probe" || blocksText(messages[2].Content[0].Content) != "actual output" {
				t.Fatalf("missing exact execution pair: %+v", messages)
			}
			if records[2].ParentUUID != records[1].UUID {
				t.Fatal("transcript parent chain broken")
			}
		})
	}
}

func TestCompleteToolTurnInterleavedParallelCalls(t *testing.T) {
	events := []llm.StreamEvent{
		{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "Probe", ToolIndex: 1, ToolIndexSet: true},
		{Type: llm.SEToolInputJSON, Text: `{"value":`, ToolIndex: 1, ToolIndexSet: true},
		{Type: llm.SEToolUseStart, ToolID: "b", ToolName: "Probe", ToolIndex: 2, ToolIndexSet: true},
		{Type: llm.SEToolInputJSON, Text: `{"value":2}`, ToolIndex: 2, ToolIndexSet: true},
		{Type: llm.SEToolInputJSON, Text: `1}`, ToolIndex: 1, ToolIndexSet: true},
		{Type: llm.SEMessageDelta, StopReason: "tool_use"},
		{Type: llm.SEMessageStop},
	}
	stream := func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
	acc := llm.NewAccumulator()
	for ev, err := range completeToolTurn(t.Context(), stream) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	calls := acc.Message().ToolUses()
	if len(calls) != 2 || string(calls[0].Input) != `{"value":1}` || string(calls[1].Input) != `{"value":2}` {
		t.Fatalf("calls=%+v", calls)
	}
}

func TestCompleteToolTurnFailPreservesCumulativeUsage(t *testing.T) {
	stream := func(yield func(llm.StreamEvent, error) bool) {
		for _, ev := range []llm.StreamEvent{
			{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: 10}},
			{Type: llm.SEToolUseStart, ToolID: "a", ToolName: "Probe"},
			{Type: llm.SEMessageDelta, Usage: llm.Usage{InputTokens: 10, OutputTokens: 3}, CumulativeUsage: true},
		} {
			if !yield(ev, nil) {
				return
			}
		}
		yield(llm.StreamEvent{}, io.ErrUnexpectedEOF)
	}
	var usage llm.Usage
	failed := false
	for ev, err := range completeToolTurn(t.Context(), stream) {
		if err != nil {
			failed = true
			break
		}
		ev.FoldUsage(&usage)
	}
	if !failed || usage.InputTokens != 10 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v failed=%v", usage, failed)
	}
}
