package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// ResolveHomeEnvVar is the home channel env var a platform plugin declares for cron delivery, empty when none
func ResolveHomeEnvVar(ctx context.Context, platform string) string {
	return resolveHomeEnvVar(ctx, platform)
}

// Delivery targets of a job under the active home, the failure routing when forFailure is set
func ResolveDeliveryTargets(ctx context.Context, job *Job, forFailure bool) []Target {
	return openCronStore(home.Dir(ctx), time.Now).resolveDeliveryTargetsFor(ctx, job, forFailure)
}
