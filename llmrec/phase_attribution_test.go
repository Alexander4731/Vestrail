package llmrec

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"os"
	"strconv"
	"testing"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

type phaseFixtureProvider struct {
	turn int
}

func (p *phaseFixtureProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.turn++
	usage := llm.Usage{InputTokens: 7, OutputTokens: 3}
	if p.turn == 1 {
		return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "fixture-list", Name: "LS", Input: []byte("{}")},
		}}, "tool_use", usage, nil
	}
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		llm.TextBlock("<summary>fixture summary</summary>"),
	}}, "end_turn", usage, nil
}

func (p *phaseFixtureProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, _ := p.Complete(ctx, req)
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart, Usage: llm.Usage{InputTokens: usage.InputTokens}}, nil) {
			return
		}
		block := msg.Content[0]
		if block.Type == llm.BlockToolUse {
			if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: block.ID, ToolName: block.Name}, nil) ||
				!yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(block.Input)}, nil) {
				return
			}
		} else if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: block.Text}, nil) {
			return
		}
		if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: llm.Usage{OutputTokens: usage.OutputTokens}}, nil) {
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func TestAuditFix_PhaseAttribution(t *testing.T) {
	dsn := os.Getenv("VESTRAIL_PG_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	lock, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	lock.SetMaxOpenConns(1)
	if _, err := lock.ExecContext(t.Context(), `SELECT pg_advisory_lock(7337741002)`); err != nil {
		t.Fatal(err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	for role, nonStreaming := range map[string]bool{"worker": false, "planner": true} {
		t.Run(role, func(t *testing.T) {
			task, err := pg.CreateTask("metering fixture", "summarize fixture", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := pg.DeleteTaskCascade(task.ID, false, false); err != nil {
					t.Error(err)
				}
			})
			session := fmt.Sprintf("exp%d-%s", task.ExplorationID, role)
			ctx := transcript.WithSessionID(t.Context(), session)
			ctx = WithTaskID(ctx, strconv.FormatInt(task.ID, 10))
			ctx = WithAttribution(ctx, task.ExplorationID, 0)
			ctx = WithPurpose(ctx, role)

			recorder := Wrap(&phaseFixtureProvider{}, pg, "metering", "fixture", "", "", func() bool { return false })
			messages := []llm.Message{llm.UserText("summarize fixture")}
			var terminal *harness.Terminal
			for event, err := range harness.Query(ctx, harness.QueryInput{
				Provider: recorder, NonStreaming: nonStreaming, Messages: messages, MaxTurns: 1,
				WorkingDir: t.TempDir(), Tools: tool.NewRegistry(tool.NewLS()),
				Settlement: &harness.Settlement{Prompt: "finish the fixture", DisabledTools: []string{"LS"}},
			}, harness.QueryDeps{}) {
				if err != nil {
					t.Fatal(err)
				}
				if event.Kind == harness.KindResult {
					terminal = event.Terminal
				}
			}
			if terminal == nil || terminal.Reason != harness.ReasonMaxTurns {
				t.Fatalf("expected a settled run, got %+v", terminal)
			}
			if _, err := compaction.ConciseProviderSummarizer(recorder, 0, nonStreaming)(ctx, messages); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := recorder.Complete(WithPurpose(ctx, "compaction"), llm.CompletionRequest{Messages: messages}); err != nil {
				t.Fatal(err)
			}
			audit, err := pg.TaskUsageAudit(t.Context(), task.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(audit.ByWorker) != 1 || audit.ByWorker[0].Key != role ||
				audit.Total.Rows != 4 || audit.Total.InputTokens != 28 || audit.Total.OutputTokens != 12 ||
				audit.ByWorker[0].Rows != audit.Total.Rows || audit.ByWorker[0].TotalTokens != audit.Total.TotalTokens {
				t.Fatalf("role totals changed: %+v", audit)
			}
			want := map[string]bool{role: true, "settlement": true, "session_compaction": true, "compaction": true}
			for _, purpose := range audit.ByPurpose {
				if !want[purpose.Key] || purpose.Rows != 1 || purpose.InputTokens != 7 || purpose.OutputTokens != 3 {
					t.Fatalf("wrong purpose totals: %+v", purpose)
				}
				delete(want, purpose.Key)
			}
			if len(want) != 0 {
				t.Fatalf("missing purposes: %v", want)
			}
		})
	}
}
