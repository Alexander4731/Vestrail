package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

type phaseWaitCompactor struct{ proactive, reactive, settlement bool }

func (p *phaseWaitCompactor) Pre(ctx context.Context, messages []llm.Message, _ int) []llm.Message {
	if p.proactive && llm.CallPurpose(ctx) != "settlement" || p.settlement && llm.CallPurpose(ctx) == "settlement" {
		<-ctx.Done()
	}
	return messages
}
func (p *phaseWaitCompactor) Reactive(ctx context.Context, messages []llm.Message) ([]llm.Message, bool) {
	if p.reactive {
		<-ctx.Done()
	}
	return messages, false
}
func (p *phaseWaitCompactor) IsOverflow(error) bool { return p.reactive }

func TestCompactionPreservesSettlementBudget(t *testing.T) {
	for _, reactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "proactive", true: "reactive"}[reactive], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			settlements := 0
			deps := QueryDeps{CallModelSync: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				if llm.CallPurpose(ctx) == "settlement" {
					settlements++
					return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("saved checkpoint")}}, "end_turn", llm.Usage{}, nil
				}
				return llm.Message{}, "", llm.Usage{}, errors.New("context overflow")
			}}
			input := QueryInput{Messages: []llm.Message{llm.UserText("fixture")}, Tools: tool.NewRegistry(), NonStreaming: true, MaxDuration: 20 * time.Millisecond, SettleDuration: 200 * time.Millisecond, Compactor: &phaseWaitCompactor{proactive: !reactive, reactive: reactive}, Settlement: &Settlement{Prompt: "finish", MaxTurns: 2}}
			var terminal *Terminal
			for event, err := range Query(ctx, input, deps) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Terminal != nil {
					terminal = event.Terminal
				}
			}
			if terminal == nil || terminal.Reason != ReasonTimeout || settlements != 1 || terminal.Text != "saved checkpoint" {
				t.Fatalf("terminal=%+v settlements=%d", terminal, settlements)
			}
			if ctx.Err() != nil {
				t.Fatal("compaction consumed parent hard deadline")
			}
		})
	}
}

func TestRequestedSettlementCompactionIsBounded(t *testing.T) {
	parent, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	ctx, request := WithSettlementRequest(parent)
	request(ReasonMaxTurns)
	request(ReasonMaxTurns)
	calls := 0
	input := QueryInput{Messages: []llm.Message{llm.UserText("fixture")}, Tools: tool.NewRegistry(), NonStreaming: true, SettleDuration: 20 * time.Millisecond, Compactor: &phaseWaitCompactor{settlement: true}, Settlement: &Settlement{Prompt: "finish", MaxTurns: 2}}
	deps := QueryDeps{CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("unexpected")}}, "end_turn", llm.Usage{}, nil
	}}
	var terminal *Terminal
	for event, err := range Query(ctx, input, deps) {
		if err != nil {
			t.Fatal(err)
		}
		if event.Terminal != nil {
			terminal = event.Terminal
		}
	}
	if calls != 0 || terminal == nil || terminal.Reason != ReasonMaxTurns || parent.Err() != nil {
		t.Fatalf("calls=%d terminal=%+v parent=%v", calls, terminal, parent.Err())
	}
}

func TestParentCancellationWinsOverSettlementRequest(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	ctx, request := WithSettlementRequest(parent)
	request(ReasonMaxTurns)
	cancel()
	calls := 0
	deps := QueryDeps{CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("unexpected")}}, "end_turn", llm.Usage{}, nil
	}}
	for _, err := range Query(ctx, QueryInput{Messages: []llm.Message{llm.UserText("fixture")}, NonStreaming: true}, deps) {
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("cancelled parent called the model")
	}
}
