package compaction

import (
	"context"
	"github.com/Autumn-27/norma/llm"
	"strings"
	"testing"
)

func TestAutoCompactPreservesLatestIntentHandoff(t *testing.T) {
	c := New(Config{ContextWindow: 200000, KeepRecent: 2, PinnedUserPrefix: "【当前执行意图 #", PinnedUserEndMarker: "\n\n【背景态势】"}, func(context.Context, []llm.Message) (string, error) { return "short summary", nil })
	msgs := []llm.Message{llm.UserText("【当前执行意图 #1】\nold"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("old done")}}, llm.UserText("【当前执行意图 #2】\nfull current brief\n\n【背景态势】\nlarge background"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("work")}}, llm.UserText("recent"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("latest")}}}
	out, changed := c.autoCompact(t.Context(), msgs, 100000, "test")
	if !changed {
		t.Fatal("not compacted")
	}
	count := 0
	for _, m := range llm.MessagesForAPI(out) {
		if strings.HasPrefix(m.Text(), "【当前执行意图 #") {
			count++
			if m.Text() != "【当前执行意图 #2】\nfull current brief" {
				t.Fatalf("wrong pin: %s", m.Text())
			}
		}
	}
	if count != 1 {
		t.Fatalf("pin count=%d", count)
	}
	if len(out) <= len(msgs) {
		t.Fatal("raw removed")
	}
}
