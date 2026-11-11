package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Whether a compression attempt recorded its signals this turn
func (a *AIAgent) LastCompressionAttemptRecorded() bool { return a.lastCompressionAttemptRecorded }

// Lock skip marker of the last compression attempt, a bool or the holder text
func (a *AIAgent) CompressionSkippedDueToLock() any { return a.compressionSkippedDueToLock }

// Transient block reason of the last compression attempt, nil when it was not blocked
func (a *AIAgent) CompressionBlockedTransient() any { return a.compressionBlockedTransient }

// Whether the last compression committed in place, nil when it aborted before commit
func (a *AIAgent) LastCompressionAttemptInPlace() *bool { return a.lastCompressionAttemptInPlace }

// Whether the last compression summary call timed out, the attribute mirror read under its writer's lock
func (a *AIAgent) LastCompressionTimedOut() bool {
	a.compressionTimeoutMu.Lock()
	defer a.compressionTimeoutMu.Unlock()
	return a.lastCompressionTimedOut
}

// Warning the last compression summary left, nil when none
func (a *AIAgent) LastCompressionSummaryWarning() *string { return a.lastCompressionSummaryWarning }

// Whether a fallback model activated during the run
func (a *AIAgent) FallbackActivated() bool { return a.fallbackActivated }

// Records one watchdog exit row for this agent's turn in its own profile
func (a *AIAgent) RecordWatchdogTurnAbort(ctx context.Context) {
	observability.RecordWatchdogTurnAbort(ctx, &a.turnExit)
}

// Whether a provider fallback serves this agent's run
func (a *AIAgent) ProviderFallbackActive() bool { return a.providerFallbackActive }
