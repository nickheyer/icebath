package logging

import (
	"context"
	"time"
)

// Record the log record factory builds now for the calling goroutine
func MakeRecord(ctx context.Context, name string, level int, msg string) *Record {
	return makeRecord(ctx, name, level, msg, time.Now(), 0)
}
