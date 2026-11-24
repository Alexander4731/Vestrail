package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
)

// HTTPStatusError retains structured rejection data for narrowly scoped fallbacks.
type HTTPStatusError struct {
	Provider string
	Status   int
	Body     string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.Provider, e.Status, e.Body)
}

func thinkingParameterRejected(err error) bool {
	var status *HTTPStatusError
	if !errors.As(err, &status) || (status.Status != 400 && status.Status != 422) {
		return false
	}
	var body struct {
		Error struct {
			Param   string `json:"param"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(status.Body), &body) != nil {
		return false
	}
	param := strings.ToLower(body.Error.Param)
	message := strings.ToLower(body.Error.Message)
	if param == "" {
		for _, field := range []string{"thinking", "reasoning_effort", "reasoning", "output_config.effort"} {
			if strings.Contains(message, field) {
				param = field
				break
			}
		}
	}
	switch param {
	case "thinking", "thinking.type", "reasoning_effort", "reasoning.effort", "reasoning", "output_config.effort":
	default:
		return false
	}
	code := strings.ToLower(body.Error.Code)
	return code == "unsupported_parameter" || code == "unknown_parameter" || code == "unrecognized_request_argument" || code == "extra_forbidden" || strings.Contains(message, "extra_forbidden") || strings.Contains(message, "unrecognized") || strings.Contains(message, "unknown field")
}

type thinkingFallbackProvider struct {
	disabled atomic.Bool
	inner    Provider
	cfg      Config
}

func (p *thinkingFallbackProvider) buildFallback(req CompletionRequest) (Provider, CompletionRequest, bool) {
	cfg := p.cfg
	cfg.ThinkingType = ""
	cfg.ReasoningEffort = ""
	req.Thinking = ""
	// Preserve the client's headers, transport and shared limiter. These copies are
	// local to this call, so concurrent calls and saved profiles are unaffected.
	switch cfg.Format {
	case FormatAnthropic:
		return &anthropicProvider{cfg: cfg}, req, true
	case FormatOpenAI:
		return &openaiProvider{cfg: cfg}, req, true
	case FormatOpenAIResponses:
		return &openaiResponsesProvider{cfg: cfg}, req, true
	}
	return nil, req, false
}

func (p *thinkingFallbackProvider) fallback(req CompletionRequest, err error) (Provider, CompletionRequest, bool) {
	if !thinkingParameterRejected(err) || (req.Thinking == "" && p.cfg.ThinkingType == "" && p.cfg.ReasoningEffort == "") {
		return nil, req, false
	}
	provider, next, ok := p.buildFallback(req)
	if !ok {
		return nil, req, false
	}
	p.disabled.Store(true)
	return provider, next, true
}
func (p *thinkingFallbackProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		provider := p.inner
		alreadyDisabled := p.disabled.Load()
		if alreadyDisabled {
			if fallback, next, ok := p.buildFallback(req); ok {
				provider, req = fallback, next
			}
		}
		visible := false
		for ev, err := range provider.Stream(ctx, req) {
			if err != nil && !alreadyDisabled && !visible && ctx.Err() == nil {
				if fallback, next, ok := p.fallback(req, err); ok {
					for ev, err := range fallback.Stream(ctx, next) {
						if !yield(ev, err) {
							return
						}
					}
					return
				}
			}
			switch ev.Type {
			case SETextDelta, SEThinkingDelta, SEToolUseStart:
				visible = true
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}
func (p *thinkingFallbackProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	provider := p.inner
	alreadyDisabled := p.disabled.Load()
	if alreadyDisabled {
		if fallback, next, ok := p.buildFallback(req); ok {
			provider, req = fallback, next
		}
	}
	msg, stop, usage, err := provider.Complete(ctx, req)
	if err != nil && !alreadyDisabled && len(msg.Content) == 0 && ctx.Err() == nil {
		if fallback, next, ok := p.fallback(req, err); ok {
			return fallback.Complete(ctx, next)
		}
	}
	return msg, stop, usage, err
}
