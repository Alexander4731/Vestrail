package compaction

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

type summaryIntegrityProvider struct {
	reply    string
	stop     string
	omitEnd  bool
	toolCall bool
	calls    int
}

func (p *summaryIntegrityProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	msg := asstText(p.reply)
	if p.toolCall {
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockToolUse, ID: "unexpected", Name: "unexpected"})
	}
	return msg, p.stop, llm.Usage{}, nil
}

func (p *summaryIntegrityProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.calls++
	return func(yield func(llm.StreamEvent, error) bool) {
		if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: p.reply}, nil) {
			return
		}
		if p.toolCall && !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "unexpected", ToolName: "unexpected"}, nil) {
			return
		}
		if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: p.stop}, nil) {
			return
		}
		if !p.omitEnd {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func TestProviderSummarizersRequireCompleteSummary(t *testing.T) {
	cases := []struct {
		name, reply, stop string
		valid             bool
	}{
		{"complete", "<summary>中文 evidence</summary>", "end_turn", true},
		{"legacy analysis", "<analysis>scratch</analysis>\n<summary>evidence</summary>", "end_turn", true},
		{"stop sequence", "<summary>evidence</summary>", "stop_sequence", true},
		{"max tokens", "<summary>evidence</summary>", "max_tokens", false},
		{"length", "<summary>evidence</summary>", "length", false},
		{"empty", "", "end_turn", false},
		{"empty body", "<summary> \n </summary>", "end_turn", false},
		{"analysis-only body", "<summary><analysis>scratch</analysis></summary>", "end_turn", false},
		{"unclosed", "<summary>partial", "end_turn", false},
		{"missing opening", "partial</summary>", "end_turn", false},
		{"duplicate", "<summary>a</summary><summary>b</summary>", "end_turn", false},
		{"plain text", "partial", "end_turn", false},
		{"unclosed analysis", "<analysis>scratch<summary>evidence</summary>", "end_turn", false},
		{"tool stop", "<summary>evidence</summary>", "tool_use", false},
		{"missing stop", "<summary>evidence</summary>", "", false},
		{"unknown stop", "<summary>evidence</summary>", "content_filter", false},
	}
	for _, build := range []struct {
		name string
		fn   func(llm.Provider, int, bool) Summarizer
	}{{"standard", ProviderSummarizer}, {"concise", ConciseProviderSummarizer}} {
		for _, nonStreaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(build.name+"/"+map[bool]string{false: "stream", true: "complete"}[nonStreaming]+"/"+tc.name, func(t *testing.T) {
					p := &summaryIntegrityProvider{reply: tc.reply, stop: tc.stop}
					got, err := build.fn(p, 0, nonStreaming)(context.Background(), []llm.Message{llm.UserText("source evidence")})
					if tc.valid {
						if err != nil || got == "" {
							t.Fatalf("complete summary rejected: %q, %v", got, err)
						}
					} else if err == nil || got != "" {
						t.Fatalf("incomplete summary accepted: %q, %v", got, err)
					}
					if p.calls != 1 {
						t.Fatalf("invalid output must not repeat the same capped request: %d calls", p.calls)
					}
				})
			}
		}
	}
}

func TestSummaryMissingStreamEndIsRejected(t *testing.T) {
	p := &summaryIntegrityProvider{reply: "<summary>evidence</summary>", stop: "end_turn", omitEnd: true}
	got, err := ProviderSummarizer(p, 0, false)(context.Background(), []llm.Message{llm.UserText("source")})
	if err == nil || got != "" {
		t.Fatalf("unterminated stream accepted: %q, %v", got, err)
	}
}

func TestFailedSummaryPreservesPreviousBoundaryAndHistory(t *testing.T) {
	p := &summaryIntegrityProvider{reply: "<summary>partial", stop: "max_tokens"}
	c := New(smallWindow(), ConciseProviderSummarizer(p, 0, false))
	msgs := []llm.Message{llm.BoundaryMessage(llm.BoundaryMeta{}), llm.UserText("previous valid summary")}
	for range 6 {
		msgs = append(msgs, bigMsg(llm.RoleUser, 4000))
	}
	before := append([]llm.Message(nil), msgs...)
	got, changed := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if changed || !reflect.DeepEqual(got, before) || !reflect.DeepEqual(msgs, before) {
		t.Fatal("failed summary replaced a valid boundary or changed original history")
	}
}

func TestEmptyCustomSummaryCannotReplaceHistory(t *testing.T) {
	c := New(smallWindow(), func(context.Context, []llm.Message) (string, error) { return " \n", nil })
	msgs := []llm.Message{bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000)}
	got, changed := c.autoCompact(context.Background(), msgs, 9000, "auto")
	if changed || !reflect.DeepEqual(got, msgs) {
		t.Fatal("empty custom summary replaced original history")
	}
}

func TestIncompleteSummaryErrorIsIdentifiable(t *testing.T) {
	p := &summaryIntegrityProvider{reply: "<summary>partial", stop: "max_tokens"}
	_, err := ProviderSummarizer(p, 0, true)(context.Background(), []llm.Message{llm.UserText("source")})
	if !errors.Is(err, ErrIncompleteSummary) {
		t.Fatalf("expected incomplete-summary error, got %v", err)
	}
}

func TestCancelledCustomSummaryCannotReplaceHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := New(smallWindow(), func(context.Context, []llm.Message) (string, error) {
		cancel()
		return "complete text after cancellation", nil
	})
	msgs := []llm.Message{bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000), bigMsg(llm.RoleUser, 4000)}
	got, changed := c.autoCompact(ctx, msgs, 9000, "auto")
	if changed || !reflect.DeepEqual(got, msgs) {
		t.Fatal("cancelled summary replaced original history")
	}
}

func TestSummaryRejectsUnexpectedToolCalls(t *testing.T) {
	for _, nonStreaming := range []bool{false, true} {
		p := &summaryIntegrityProvider{reply: "<summary>evidence</summary>", stop: "end_turn", toolCall: true}
		got, err := ProviderSummarizer(p, 0, nonStreaming)(context.Background(), []llm.Message{llm.UserText("source")})
		if got != "" || !errors.Is(err, ErrIncompleteSummary) {
			t.Fatalf("nonStreaming=%v: tool-bearing summary accepted: %q, %v", nonStreaming, got, err)
		}
	}
}
