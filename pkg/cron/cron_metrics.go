package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Counts one dropped scheduled occurrence on the profile the due scan runs under
var recordCronMissed = func(ctx context.Context, job *Job) {
	observability.RecordCronMissed(ctx, map[string]any{"deliver": nullable(job.Deliver)})
}
