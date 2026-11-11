package pyctx

import "context"

// Copy of the context as a fresh worker thread sees it, no event loop running there
func WithoutRunningLoop(ctx context.Context) context.Context {
	return context.WithValue(ctx, runningLoopKey{}, false)
}
