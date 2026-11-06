package pyctx

import "context"

type runningLoopKey struct{}

// Marks a context as served by a goroutine running an event loop, asyncio.get_running_loop succeeding
func WithRunningLoop(ctx context.Context) context.Context {
	return context.WithValue(ctx, runningLoopKey{}, true)
}

// Whether the context was marked as running an event loop
func RunningLoop(ctx context.Context) bool {
	v, _ := ctx.Value(runningLoopKey{}).(bool)
	return v
}
