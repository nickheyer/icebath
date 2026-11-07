package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Submits fn to loop from synchronous code, nil with a logged reason when the loop is missing or refuses work
func SafeScheduleThreadsafe(ctx context.Context, loop *DaemonPool, fn func(ctx context.Context) (any, error), log *slog.Logger, logMessage string, level slog.Level) *PoolFuture {
	if log == nil {
		log = slog.Default().With("logger", "agent.async_utils")
	}
	if logMessage == "" {
		logMessage = "Failed to schedule coroutine on loop"
	}
	var err error
	if loop == nil {
		err = errors.New("loop is None")
	} else {
		var f *PoolFuture
		if f, err = loop.Submit(ctx, fn); err == nil {
			return f
		}
	}
	log.Log(ctx, level, fmt.Sprintf("%s: %s", logMessage, err))
	return nil
}

// Observes a detached future's outcome once it settles so its error or cancellation is retrieved and discarded
func ConsumeDetachedTaskResult(f *PoolFuture) {
	if f == nil {
		return
	}
	go func() {
		<-f.Done()
		_, _ = f.Result(0)
	}()
}
