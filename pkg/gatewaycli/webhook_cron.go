//go:build !icebath_core && !icebath_noextras

package gatewaycli

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Cron reference resolver of webhook routes, swapped by tests
var webhookCronJobRef = cron.ResolveJobRef

func init() {
	webhookRouteCronJob = func(ctx context.Context, h string, profile any, reference string) (string, string, error) {
		job, err := resolveRouteCronJob(ctx, h, profile, reference)
		var ambiguous *cron.AmbiguousJobReferenceError
		if errors.As(err, &ambiguous) {
			return "", err.Error(), nil
		}
		if err != nil || job == nil {
			return "", "", err
		}
		return job.ID, "", nil
	}
}

// Resolves a cron reference in the route profile store, the caller home for the default profile
func resolveRouteCronJob(ctx context.Context, h string, profile any, reference string) (*cron.Job, error) {
	if pyrepr.Equal(profile, "default") {
		return webhookCronJobRef(ctx, h, reference)
	}
	dir, err := profiles.GetProfileDir(pyrepr.Str(profile))
	if err != nil {
		return nil, err
	}
	return webhookCronJobRef(ctx, dir, reference)
}
