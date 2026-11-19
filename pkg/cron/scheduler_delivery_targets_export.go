package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Delivery targets of a job under the active home, the failure routing when forFailure is set
func ResolveDeliveryTargets(ctx context.Context, job *Job, forFailure bool) []Target {
	return openCronStore(home.Dir(ctx), time.Now).resolveDeliveryTargetsFor(ctx, job, forFailure)
}
