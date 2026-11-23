package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

func TestAuditFix_MaxTokensContract(t *testing.T) {
	const chatResponse = `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	tests := []struct {
		format                            llm.Format
		tokenField, fieldOption, response string
		defaultLimit                      int
	}{
		{llm.FormatOpenAI, "max_tokens", "", chatResponse, 0},
		{llm.FormatOpenAI, "max_completion_tokens", llm.MaxTokensFieldCompletion, chatResponse, 0},
		{llm.FormatOpenAIResponses, "max_output_tokens", "",
			`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`, 0},
		{llm.FormatAnthropic, "max_tokens", "",
			`{"stop_reason":"end_turn","content":[{"type":"text","text":"ok"}]}`, 32768},
	}
	for _, test := range tests {
		for _, limit := range []int{0, 2048} {
			t.Run(fmt.Sprintf("%s/%s/%d", test.format, test.tokenField, limit), func(t *testing.T) {
				bodies := make(chan map[string]json.RawMessage, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "invalid fixture request", http.StatusBadRequest)
						return
					}
					bodies <- body
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, test.response)
				}))
				defer server.Close()

				cfg := llm.Config{
					Format: test.format, Model: "fixture", BaseURL: server.URL,
					MaxTokensField: test.fieldOption, MaxRetries: -1,
				}
				if test.format == llm.FormatAnthropic {
					cfg.ThinkingType = "enabled"
				}
				provider, err := llm.NewProvider(cfg)
				if err != nil {
					t.Fatal(err)
				}
				terminal := drain(t, QueryInput{
					Provider: provider, NonStreaming: true, MaxTokens: limit,
					Messages: []llm.Message{llm.UserText("say ok")},
				}, QueryDeps{})
				if terminal.Reason != ReasonCompleted || terminal.Text != "ok" {
					t.Fatalf("unexpected terminal: %+v", terminal)
				}
				body := <-bodies
				expected := limit
				if expected == 0 {
					expected = test.defaultLimit
				}
				for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
					raw, present := body[key]
					if key != test.tokenField || expected == 0 {
						if present {
							t.Errorf("unexpected %s=%s", key, raw)
						}
						continue
					}
					if string(raw) != fmt.Sprint(expected) {
						t.Errorf("%s=%s, want %d", key, raw, expected)
					}
				}
			})
		}
	}
}
