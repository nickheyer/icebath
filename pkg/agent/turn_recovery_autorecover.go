package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Ladder wait and backoff steps, replaced by tests
var (
	ladderBackoffSleep    = interruptibleBackoffSleep
	ladderJitteredBackoff = provider.JitteredBackoff
)

// Configured agent auto recovery cycles, zero disables the ladder
func autoRecoveryCycles(a *AIAgent) int { return max(a.autoRecoveryCycles, 0) }

// Seconds before a one based cycle, the provider retry after capped at 120 s or the 15 30 60 jitter
func ladderWaitSeconds(cycle int, apiError error) float64 {
	if retryAfter, ok := providerRetryAfterSeconds(apiError); ok {
		return min(retryAfter.Seconds(), 120)
	}
	return ladderJitteredBackoff(cycle, provider.BackoffOptions{BaseDelay: 15 * time.Second, MaxDelay: 60 * time.Second, JitterRatio: 0.2}).Seconds()
}

// True for configured cycles, a transient transport verdict, and no answer text streamed to the user yet
func ladderEligible(a *AIAgent, classified *provider.Classified) bool {
	if autoRecoveryCycles(a) <= 0 || !provider.LadderReason(classified.Reason) {
		return false
	}
	return !a.hasContentAfterThinkBlock(a.currentStreamedAssistantText())
}

// Countdown line naming the wait, the cycle and how this surface stops it
func ladderNotice(a *AIAgent, waitS float64, cycle, total int) string {
	hint := provider.LadderStopHint(a.platform)
	text := fmt.Sprintf("⏳ Provider temporarily unavailable — retrying automatically in %.0fs (cycle %d/%d)", waitS, cycle, total)
	if hint != "" {
		return text + "; " + hint
	}
	return text
}

// Waits out one recovery cycle after retries and fallback are spent, nil when the ladder declines or is spent
func autoRecoverAfterExhaustion(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn) (*PhaseVerdict, error) {
	if !ladderEligible(a, e.classified) {
		return nil, nil
	}
	retry := s.Retry
	total := autoRecoveryCycles(a)
	used := retry.AutoRecoveryCyclesUsed
	if used >= total {
		a.emitDiagnosticStatus(fmt.Sprintf("⏳ Automatic recovery gave up after %d cycles — the provider is still unavailable.", total))
		return nil, nil
	}
	cycle := used + 1
	retry.AutoRecoveryCyclesUsed = cycle
	waitS := ladderWaitSeconds(cycle, e.apiError)
	notice := ladderNotice(a, waitS, cycle, total)
	a.emitDiagnosticStatus(notice)
	a.emitDiagnosticWait(notice)
	conversationLoopLog().Warn(fmt.Sprintf("%sProvider unavailable (%s) — auto-recovery cycle %d/%d, retrying in %.0fs %s",
		a.logPrefix, e.classified.Reason, cycle, total, waitS, a.ClientLogContext()))
	interrupted, err := ladderBackoffSleep(ctx, a, waitS, retry, &s.Messages, s.ConversationHistory, s.APICallCount,
		"Interrupt detected during automatic recovery wait, aborting.",
		fmt.Sprintf("Operation interrupted: waiting for the provider to recover (cycle %d/%d).", cycle, total),
		fmt.Sprintf("auto-recovery wait (%d/%d)", cycle, total))
	if err != nil {
		return nil, err
	}
	if interrupted != nil {
		return &PhaseVerdict{Action: "return", Result: interrupted}, nil
	}
	if retry.RestartWithRedirectedMessages {
		return &PhaseVerdict{Action: "break"}, nil
	}
	return &PhaseVerdict{Action: "continue"}, nil
}
