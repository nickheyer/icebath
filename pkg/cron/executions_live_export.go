package cron

import (
	"context"
	"time"
)

// Moves an attempt to running in the executions ledger of the given home, nil when it was not claimed
func MarkExecutionRunningIn(ctx context.Context, home, executionID string) (map[string]any, error) {
	return openExecutionsLedger(openCronStore(home, time.Now)).MarkRunning(ctx, executionID)
}

// Claimed or running attempt of a job under a live owner in the executions ledger of the given home
func LiveInflightExecutionIn(ctx context.Context, home, jobID string) (map[string]any, error) {
	return openExecutionsLedger(openCronStore(home, time.Now)).LiveInflight(ctx, jobID)
}
