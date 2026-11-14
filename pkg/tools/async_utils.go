package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Submits fn to loop from synchronous code, nil with a logged reason when the loop is missing or refuses work
func SafeScheduleThreadsafe(ctx context.Context, loop *DaemonPool, fn func(ctx context.Context) (any, error), log *slog.Logger, logMessage string, level slog.Level) *PoolFuture {
	var err error
	if loop == nil {
		err = ErrLoopIsNone
	} else {
		var f *PoolFuture
		if f, err = loop.Submit(ctx, fn); err == nil {
			return f
		}
	}
	LogScheduleFailure(ctx, log, logMessage, level, err)
	return nil
}

// Scheduling failure of a missing loop
var ErrLoopIsNone = errors.New("loop is None")

// Scheduling failure asyncio reports for a loop that already closed
var ErrEventLoopClosed = errors.New("Event loop is closed")

// Logs why work could not be scheduled, the default logger and message standing in when none is given
func LogScheduleFailure(ctx context.Context, log *slog.Logger, logMessage string, level slog.Level, err error) {
	if log == nil {
		log = slog.Default().With("logger", "agent.async_utils")
	}
	if logMessage == "" {
		logMessage = "Failed to schedule coroutine on loop"
	}
	log.Log(ctx, level, fmt.Sprintf("%s: %s", logMessage, err))
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
