package gateway

// Cancels a cold refine review sharing the session id before the next turn runs, waiting a bounded time
func cancelLiveReviewForTurn(live *LiveSession) {
	live.ReviewRun.CancelForLiveTurn("superseded by a new live turn", "background review superseded")
}
