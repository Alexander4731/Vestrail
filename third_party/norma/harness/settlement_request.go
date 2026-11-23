package harness

import (
	"context"
	"sync"
	"time"
)

type settlementRequestKey struct{}
type completionRequestKey struct{}

type completionRequest struct {
	mu        sync.Mutex
	requested bool
	summary   string
}

// WithCompletionRequest lets a host end a run after its current tool results
// have been recorded, without requesting another model turn.
func WithCompletionRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, completionRequestKey{}, &completionRequest{})
}

func RequestCompletion(ctx context.Context, summary string) {
	if request, ok := ctx.Value(completionRequestKey{}).(*completionRequest); ok {
		request.mu.Lock()
		if !request.requested {
			request.requested, request.summary = true, summary
		}
		request.mu.Unlock()
	}
}

func requestedCompletion(ctx context.Context) (string, bool) {
	if request, ok := ctx.Value(completionRequestKey{}).(*completionRequest); ok {
		request.mu.Lock()
		defer request.mu.Unlock()
		return request.summary, request.requested
	}
	return "", false
}

type phaseDeadlinesKey struct{}
type phaseDeadlines struct{ explore, settle time.Time }

func WithPhaseDeadlines(ctx context.Context, explore, settle time.Time) context.Context {
	return context.WithValue(ctx, phaseDeadlinesKey{}, phaseDeadlines{explore, settle})
}

func ExplorationDeadline(ctx context.Context) (time.Time, bool) {
	phases, ok := ctx.Value(phaseDeadlinesKey{}).(phaseDeadlines)
	return phases.explore, ok && !phases.explore.IsZero()
}

// WithSettlementRequest asks the next loop boundary to enter bounded settlement
// while keeping the parent context live for result write-back.
func WithSettlementRequest(ctx context.Context) (context.Context, func(TerminalReason)) {
	requests := make(chan TerminalReason, 1)
	var once sync.Once
	return context.WithValue(ctx, settlementRequestKey{}, requests), func(reason TerminalReason) {
		once.Do(func() { requests <- reason })
	}
}

func takeSettlementRequest(ctx context.Context) (TerminalReason, bool) {
	if requests, ok := ctx.Value(settlementRequestKey{}).(chan TerminalReason); ok {
		select {
		case reason := <-requests:
			return reason, true
		default:
		}
	}
	return "", false
}
