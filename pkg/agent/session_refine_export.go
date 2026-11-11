package agent

import "context"

// Cancellation and request completion handshake of one background review
type BackgroundReviewRun = backgroundReviewRun

// Fresh review run token for a guard that is not an agent, a local session's review slot
func NewBackgroundReviewRun() *BackgroundReviewRun { return &backgroundReviewRun{} }

// Latches request completion once, true for the caller that latched it
func (r *backgroundReviewRun) MarkRequestFinished() bool { return r.markRequestFinished() }

// Publishes the run's request exit to every waiter
func (r *backgroundReviewRun) SetRequestDone() { r.requestDone.Set() }

// Blocks until the run's request phase has exited
func (r *backgroundReviewRun) WaitRequestDone() { r.requestDone.Wait(-1) }

// Spawns or idle queues a background review with the focus text and explicit flag of a refine request
func (a *AIAgent) SpawnBackgroundReview(ctx context.Context, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string, explicit bool) {
	a.spawnBackgroundReview(ctx, messagesSnapshot, reviewMemory, reviewSkills, focus, explicit)
}

// Review run installed on the agent, nil when none is
func (a *AIAgent) CurrentBackgroundReviewRun() *BackgroundReviewRun {
	return currentBackgroundReviewRun(a)
}

// Review worker target and its prompt for the snapshot, the focus appended and the task config read once
func SpawnBackgroundReviewThread(ctx context.Context, a *AIAgent, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool,
	focus *string, run *BackgroundReviewRun, explicit bool) (func(context.Context), string, error) {
	return spawnBackgroundReviewThread(ctx, a, backgroundReviewKwargs{messagesSnapshot: messagesSnapshot, reviewMemory: reviewMemory,
		reviewSkills: reviewSkills, focus: focus, explicit: explicit}, run)
}

// Structural clone of a review snapshot so the fork never aliases the live history
func CloneBackgroundReviewMessages(messages []map[string]any) []map[string]any {
	return cloneBackgroundReviewMessages(messages)
}
