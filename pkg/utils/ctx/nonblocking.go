package ctxutil

import "context"

type nonBlockingKey struct{}

// Context marked as serving asynchronous dispatch, where callees must not park the goroutine on blocking waits; NonBlocking reports the mark
func WithNonBlocking(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonBlockingKey{}, true)
}

// Context with the non-blocking mark lifted, as a worker goroutine started off a marked context runs: blocking waits are fine there
func WithBlocking(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonBlockingKey{}, false)
}

// Whether ctx carries the non-blocking mark WithNonBlocking sets and WithBlocking lifts
func NonBlocking(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(nonBlockingKey{}).(bool)
	return marked
}
