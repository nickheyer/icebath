package cron

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Jobs of a home's store, disabled ones included when asked
func ListJobs(ctx context.Context, h string, includeDisabled bool) ([]*Job, error) {
	return openCronStore(h, nil).listJobs(ctx, includeDisabled)
}

// Applies a field update to a job, nil when the job is gone
func UpdateJob(ctx context.Context, h, jobID string, updates map[string]any) (*Job, error) {
	return openCronStore(h, nil).updateJob(ctx, jobID, updates)
}

// Removes a job, false when none matched
func RemoveJob(ctx context.Context, h, ref string) (bool, error) {
	return openCronStore(h, nil).removeJob(ctx, ref)
}

// Triggers a job now with an extra prompt appended to this run only
func TriggerJobWithPrompt(ctx context.Context, h, ref, extraPrompt string) (*Job, error) {
	return openCronStore(h, nil).triggerJob(ctx, ref, extraPrompt)
}

// Creates a job and registers it with the active scheduler provider
func CreateJobWithSchedulerRegistration(ctx context.Context, h string, spec *pyjson.Object) (*Job, error) {
	return openCronStore(h, nil).createJobWithSchedulerRegistration(ctx, spec)
}

// Prompt injection finding of a cron prompt, empty when clean
func ScanCronPrompt(prompt string) string { return scanCronPrompt(prompt) }

// Notifies the active scheduler provider that a home's jobs changed
func NotifyProviderJobsChanged(h string) { openCronStore(h, nil).notifyProviderJobsChanged() }

// Active scheduler provider of a home's store
func ResolveCronScheduler(h string) CronScheduler { return resolveCronScheduler(openCronStore(h, nil)) }

// Whether a provider claims and fires in separate steps
func ProviderSupportsSplitFire(p CronScheduler) bool { return providerSupportsSplitFire(p) }

// Claims then runs one job through a provider
func SchedulerFireDue(ctx context.Context, p CronScheduler, jobID string, o FireOptions) (bool, error) {
	return schedulerFireDue(ctx, p, jobID, o)
}

// Durably claims one fire through a provider
func SchedulerClaimFire(ctx context.Context, p CronScheduler, jobID string) (*Job, error) {
	return schedulerClaimFire(ctx, p, jobID, false, false)
}

// Runs a claimed job snapshot through a provider
func SchedulerFireClaimed(ctx context.Context, p CronScheduler, job *Job, o FireOptions) (bool, error) {
	return schedulerFireClaimed(ctx, p, job, o)
}
