package logging

import (
	"context"
	"time"
)

// Routes slog through the logger hierarchy from process start, so a record before setup meets the last resort handler
func InstallSlogBridge() { installSlogDefault() }

// Record the log record factory builds now for the calling goroutine
func MakeRecord(ctx context.Context, name string, level int, msg string) *Record {
	return makeRecord(ctx, name, level, msg, time.Now(), 0)
}
