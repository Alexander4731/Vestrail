package harness

import (
	"context"
	"iter"
	"testing"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
)

type slowExploreThenSettleProvider struct {
	calls          int
	settlePurposes []string
}

func (p *slowExploreThenSettleProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	if p.calls == 1 {
		<-ctx.Done()
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}
	p.settlePurposes = append(p.settlePurposes, llm.CallPurpose(ctx))
	return llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.TextBlock("settled summary written")},
	}, "end_turn", llm.Usage{InputTokens: 10, OutputTokens: 5}, nil
}

func (p *slowExploreThenSettleProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: usage.InputTokens}}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: msg.Text()}, nil) {
			return
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: usage.OutputTokens}}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func TestAuditFix_ExploreStreamTimeoutEntersSettlement(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		prov := &slowExploreThenSettleProvider{}
		term := drain(t, QueryInput{
			Provider:       prov,
			NonStreaming:   nonStreaming,
			Messages:       []llm.Message{llm.UserText("start")},
			Tools:          tool.NewRegistry(),
			MaxDuration:    40 * time.Millisecond,
			SettleDuration: 2 * time.Second,
			Settlement: &Settlement{
				Prompt:   "write back and finish",
				MaxTurns: 2,
			},
		}, QueryDeps{})
		if term.Reason != ReasonTimeout {
			t.Fatalf("nonStreaming=%v: got reason %q, want %q (err=%v)", nonStreaming, term.Reason, ReasonTimeout, term.Err)
		}
		if term.Text != "settled summary written" {
			t.Fatalf("nonStreaming=%v: settlement did not complete, got text %q", nonStreaming, term.Text)
		}
		if prov.calls != 2 || len(prov.settlePurposes) != 1 || prov.settlePurposes[0] != "settlement" {
			t.Fatalf("nonStreaming=%v: unexpected calls=%d settlePurposes=%v", nonStreaming, prov.calls, prov.settlePurposes)
		}
	}
}
