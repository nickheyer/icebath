package cron

import (
	"context"
	"time"
)

// Persists a claimed attempt of a job in the executions ledger of the given home
func CreateExecutionIn(ctx context.Context, home, jobID, source string) (map[string]any, error) {
	return openExecutionsLedger(openCronStore(home, time.Now)).Create(ctx, jobID, source, nil)
}

// Settles an attempt as finished in the executions ledger of the given home
func FinishExecutionIn(ctx context.Context, home, executionID string, success bool) (map[string]any, error) {
	return openExecutionsLedger(openCronStore(home, time.Now)).Finish(ctx, executionID, success, nil, nil, false)
}
