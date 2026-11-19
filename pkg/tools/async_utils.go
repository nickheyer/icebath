package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Scheduling failure of a missing loop
var ErrLoopIsNone = errors.New("loop is None")

// Scheduling failure asyncio reports for a loop that already closed
var ErrEventLoopClosed = errors.New("Event loop is closed")

// Logs why work could not be scheduled, the default logger and message standing in when none is given
func LogScheduleFailure(ctx context.Context, log *slog.Logger, logMessage string, level slog.Level, err error) {
	if log == nil {
		log = pkgLog()
	}
	if logMessage == "" {
		logMessage = "Failed to schedule coroutine on loop"
	}
	log.Log(ctx, level, fmt.Sprintf("%s: %s", logMessage, err))
}
