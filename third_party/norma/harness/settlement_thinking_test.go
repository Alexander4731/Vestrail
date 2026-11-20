package harness

import (
	"context"
	"encoding/json"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
	"iter"
	"testing"
)

func TestSettlementDisablesThinking(t *testing.T) {
	fixture := tool.Build(tool.Spec{Name: "fixture", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		return tool.Text("ok"), nil
	}})
	for _, preserve := range []bool{false, true} {
		var reqs []llm.CompletionRequest
		call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			reqs = append(reqs, req)
			evs := textTurn("done")
			if len(reqs) == 1 {
				evs = toolTurn("call", "fixture", `{}`)
			}
			return func(yield func(llm.StreamEvent, error) bool) {
				for _, e := range evs {
					if !yield(e, nil) {
						return
					}
				}
			}
		}
		in := QueryInput{Tools: tool.NewRegistry(fixture), PermissionMode: permission.ModeBypass, Messages: []llm.Message{llm.UserText("go")}, MaxTurns: 1, Settlement: &Settlement{Prompt: "wrap up", DisabledTools: []string{"fixture"}, PreserveThinking: preserve}}
		term := drain(t, in, QueryDeps{CallModel: call})
		if term.Reason != ReasonMaxTurns || len(reqs) != 2 {
			t.Fatalf("reason=%v calls=%d", term.Reason, len(reqs))
		}
		if reqs[0].Thinking != "" {
			t.Fatal("main thinking overridden")
		}
		if !preserve && reqs[1].Thinking != "disabled" {
			t.Fatal("settlement thinking retained")
		}
		if preserve && reqs[1].Thinking != "" {
			t.Fatal("opt out ignored")
		}
	}
}
