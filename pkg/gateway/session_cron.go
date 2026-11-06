package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// CronOwnerExecution is the cron admission a context runs under, the owner's exact store and the occurrence ids
type CronOwnerExecution struct {
	DB          *state.DB
	DBPath      string
	SessionID   string
	JobID       string
	ExecutionID string
}

type cronExecutionKey struct{}

// Cron owner execution bound to ctx, false outside a canonical cron run
func CurrentCronExecution(ctx context.Context) (CronOwnerExecution, bool) {
	exec, ok := ctx.Value(cronExecutionKey{}).(CronOwnerExecution)
	return exec, ok
}

// Context carrying the cron admission of an owner's store, session, job and execution
func BindCronExecution(ctx context.Context, exec CronOwnerExecution) context.Context {
	return context.WithValue(ctx, cronExecutionKey{}, exec)
}
