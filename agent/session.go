package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strings"

	"github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
)

func newAgentSession(opts agentcore.Options) *agentcore.Session {
	if !OptimizationEnabled("VESTRAIL_OPT_REASONING_HISTORY") {
		opts.PreserveReasoningHistory = true
	}
	if opts.Tools == nil {
		opts.Tools = tool.DefaultTools()
	}
	// The pinned SDK now supplies exact call IDs to pre-hooks for every tool,
	// including SDK-owned tools. Admission must precede execution observation.
	call := opts.Deps.CallModel
	if call == nil && opts.Provider != nil {
		call = opts.Provider.Stream
	}
	if call != nil {
		opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
			return completeToolTurn(ctx, call(ctx, req))
		}
	}
	return agentcore.NewSession(opts)
}

// norma v0.4.3 dispatches tools while reading the model stream, but skips result
// collection and transcript writes if that stream fails. Hold the tool-bearing
// tail until the response succeeds; the SDK then owns execution and persistence.
// Text and thinking before the first tool continue to stream normally.
func completeToolTurn(ctx context.Context, stream iter.Seq2[llm.StreamEvent, error]) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		var pending []llm.StreamEvent
		finished := false
		fail := func(err error) {
			// Discarded calls still consumed tokens. Preserve their usage without
			// forwarding any tool input or a successful stop reason to the SDK.
			for _, ev := range pending {
				if ev.Type == llm.SEMessageStart || ev.Type == llm.SEMessageDelta {
					if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, Usage: ev.Usage, CumulativeUsage: ev.CumulativeUsage}, nil) {
						return
					}
				}
			}
			yield(llm.StreamEvent{}, err)
		}
		for ev, err := range stream {
			if err != nil {
				fail(err)
				return
			}
			if ev.Type == llm.SEMessageDelta && ev.StopReason != "" {
				finished = true
			}
			if ev.Type == llm.SEToolUseStart || pending != nil {
				pending = append(pending, ev)
			} else if !yield(ev, nil) {
				return
			}
		}
		if pending == nil {
			return
		}
		if err := ctx.Err(); err != nil {
			fail(err)
			return
		}
		// Some providers treat a clean transport EOF as success even when the
		// model's terminal delta never arrived. Complete JSON alone is insufficient.
		if !finished {
			fail(io.ErrUnexpectedEOF)
			return
		}
		// Validate the whole batch before dispatching its first call. The SDK
		// otherwise replaces malformed/truncated arguments with an empty object.
		buffers := map[int]*strings.Builder{}
		var order []int
		lastIdx := -1
		callIDs := map[string]bool{}
		for _, ev := range pending {
			switch ev.Type {
			case llm.SEToolUseStart:
				if ev.ToolID == "" || callIDs[ev.ToolID] {
					fail(fmt.Errorf("model returned a missing or duplicate tool call ID"))
					return
				}
				callIDs[ev.ToolID] = true
				idx := ev.ToolIndex
				if !ev.ToolIndexSet {
					idx = len(order)
				}
				if buffers[idx] != nil {
					fail(fmt.Errorf("model returned a duplicate tool call index"))
					return
				}
				buffers[idx] = &strings.Builder{}
				order = append(order, idx)
				lastIdx = idx
			case llm.SEToolInputJSON:
				if ev.ToolIndexSet && buffers[ev.ToolIndex] != nil {
					buffers[ev.ToolIndex].WriteString(ev.Text)
				} else if lastIdx >= 0 {
					buffers[lastIdx].WriteString(ev.Text)
				}
			}
		}
		for _, idx := range order {
			b := buffers[idx]
			if b.Len() > 0 && !json.Valid([]byte(b.String())) {
				fail(fmt.Errorf("model returned incomplete or invalid tool arguments"))
				return
			}
		}
		for _, ev := range pending {
			if !yield(ev, nil) {
				return
			}
		}
	}
}
