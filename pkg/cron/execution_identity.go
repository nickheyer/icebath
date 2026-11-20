package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CronExecution identifies the scheduled occurrence the current code runs inside
type CronExecution struct {
	JobID            string
	JobName          string
	ExecutionID      string
	Source           string
	ScheduledInstant *string
	StartedAt        *string
	Profile          string
}

type cronExecutionKey struct{}

// Binds the fire's identity from the ledger row of its running transition
func enterCronExecution(ctx context.Context, job *Job, executionID string, record map[string]any) context.Context {
	name := job.Name
	if name == "" {
		name = job.ID
	}
	source := ""
	if pyTruthy(record["source"]) {
		source = pyStrOrEmpty(record["source"])
	}
	profile := profiles.CurrentProfileName(ctx)
	if profile == "" {
		profile = "default"
	}
	return context.WithValue(ctx, cronExecutionKey{}, &CronExecution{
		JobID:            job.ID,
		JobName:          name,
		ExecutionID:      executionID,
		Source:           source,
		ScheduledInstant: optString(record["scheduled_instant"]),
		StartedAt:        optString(record["started_at"]),
		Profile:          profile,
	})
}

// CurrentCronExecution is the occurrence bound to ctx, nil outside a fire and inside delegated children
func CurrentCronExecution(ctx context.Context) *CronExecution {
	if tools.IsDelegatedChildContext(ctx) {
		return nil
	}
	exec, _ := ctx.Value(cronExecutionKey{}).(*CronExecution)
	return exec
}

// String value of an optional ledger column
func optString(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}
