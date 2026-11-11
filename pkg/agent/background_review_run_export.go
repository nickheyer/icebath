package agent

import "fmt"

// True once the run's request phase has exited
func (r *backgroundReviewRun) RequestDone() bool { return r.requestDone.IsSet() }

// Cancels this review run for a new live turn and waits a bounded time for its acknowledgement
func (r *backgroundReviewRun) CancelForLiveTurn(message, toolReason string) {
	if reviewAgent := r.cancel(); reviewAgent != nil {
		interruptBackgroundReview(reviewAgent, message, toolReason)
	}
	if !awaitReviewAck(r, backgroundReviewCancelTimeoutSeconds) {
		backgroundReviewLog().Warn(fmt.Sprintf("Background review did not acknowledge cancellation within %.1fs; proceeding (%s)",
			backgroundReviewCancelTimeoutSeconds, message))
	}
}
