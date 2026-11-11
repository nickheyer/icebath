package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// ClaimedTurnStopped reports whether a Stop is latched or delivered for the claimed owner generation
func (a *SessionAuthority) ClaimedTurnStopped(owner string, generation int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if stop, ok := a.pendingStops[owner]; ok && stop == generation {
		return true
	}
	stop, ok := a.deliveredStops[owner]
	return ok && stop == generation
}

// RecordInterruptedExecution settles the executing admission of the context as an interrupted empty turn
func RecordInterruptedExecution(ctx context.Context) {
	if captured := executionResultOf(ctx); captured != nil {
		captured.Set("result", pyjson.ObjectOf("final_response", "", "messages", []any{}, "interrupted", true))
	}
}

// RecordUnexecutedFailure commits a failed result for the executing admission of a turn that never ran, returning the reply
func RecordUnexecutedFailure(ctx context.Context, reply, reason string, failure any) string {
	return recordUnexecutedFailure(ctx, reply, reason, failure)
}
