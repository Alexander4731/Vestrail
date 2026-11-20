package agentcore

import (
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSessionForkFromZeroUsage(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("source", "")
	msgs := []llm.Message{llm.UserText("fixture"), {Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "chain thought"}, {Type: llm.BlockToolUse, ID: "read", Name: "Read", Input: []byte(`{}`)}}}, {Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", strings.Repeat("raw", 10000), false)}}, {Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("done")}}}
	for i, m := range msgs {
		u := llm.Usage{}
		if i == 1 {
			u = llm.Usage{InputTokens: 40000, CacheReadTokens: 30000, OutputTokens: 20}
		}
		w.RecordMessage(m, u)
	}
	working := append([]llm.Message(nil), msgs...)
	working[2] = llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("read", "[Old tool result content cleared]", false)}}
	if e := store.SaveWorkingMessages("source", w.LastUUID(), working); e != nil {
		t.Fatal(e)
	}
	info, e := InspectTranscript(store, "source")
	if e != nil || !info.Exists || !info.HasWorkingSnapshot || info.LastInputTokens != 40000 {
		t.Fatalf("info=%+v err=%v", info, e)
	}
	s := NewSession(Options{Transcript: store, SessionID: "fork", Tools: []tool.CoreTool{}, DisableBackgroundTasks: true})
	defer s.Close()
	if e = s.ForkFrom("source", "fork"); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(s.Messages(), working) || s.Usage() != (llm.Usage{}) {
		t.Fatal("fork changed working messages or usage")
	}
	recs, e := store.Load(store.MainPath("fork"))
	if e != nil {
		t.Fatal(e)
	}
	_, u := transcript.Messages(recs)
	if u != (llm.Usage{}) {
		t.Fatalf("seed usage=%+v", u)
	}
	for i, r := range recs {
		if r.UUID == "" {
			t.Fatal("missing UUID")
		}
		if i > 0 && r.ParentUUID != recs[i-1].UUID {
			t.Fatal("broken chain")
		}
	}
	before, e := os.ReadFile(store.MainPath("fork"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.ForkFrom("source", "fork"); e == nil {
		t.Fatal("destination overwritten")
	}
	after, _ := os.ReadFile(store.MainPath("fork"))
	if string(before) != string(after) {
		t.Fatal("old file changed")
	}
	if e = s.ForkFrom("missing", "fresh"); e == nil {
		t.Fatal("missing source accepted")
	}
	if e = s.ForkFrom("source", "../escape"); e == nil {
		t.Fatal("path escape accepted")
	}
}
func TestWorkingCheckpointResumeAndStaleFallback(t *testing.T) {
	store := transcript.NewStore(t.TempDir())
	w := store.NewWriter("source", "")
	w.RecordMessage(llm.UserText("raw"), llm.Usage{})
	if e := store.SaveWorkingMessages("source", w.LastUUID(), []llm.Message{llm.UserText("working")}); e != nil {
		t.Fatal(e)
	}
	s := NewSession(Options{Transcript: store, SessionID: "source", Tools: []tool.CoreTool{}, DisableBackgroundTasks: true})
	defer s.Close()
	if e := s.Resume("source"); e != nil {
		t.Fatal(e)
	}
	if s.Messages()[0].Text() != "working" {
		t.Fatal("fresh snapshot ignored")
	}
	w.RecordMessage(llm.UserText("next"), llm.Usage{})
	if e := s.Resume("source"); e != nil {
		t.Fatal(e)
	}
	if len(s.Messages()) != 2 || s.Messages()[0].Text() != "raw" {
		t.Fatal("stale snapshot used")
	}
	if e := os.WriteFile(store.MainPath("source")+".working.json", []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := s.Resume("source"); e == nil {
		t.Fatal("corruption ignored")
	}
}
