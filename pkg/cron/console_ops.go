package cron

import (
	"context"
	"io"
)

// Creates a job in the cron store of the home, cron/jobs.py create_job
func CreateJob(ctx context.Context, h string, o CreateJobOptions) (*Job, error) {
	return openCronStore(h, nil).createJob(ctx, o)
}

// Job of the home by exact id, nil when absent, cron/jobs.py get_job
func GetJob(ctx context.Context, h, jobID string) (*Job, error) {
	return openCronStore(h, nil).getJob(ctx, jobID)
}

// Pauses a job of the home by id or name with the reason, nil when nothing matches
func PauseJob(ctx context.Context, h, ref string, reason *string) (*Job, error) {
	return openCronStore(h, nil).pauseJob(ctx, ref, reason)
}

// Resumes a paused job of the home by id or name, nil when nothing matches
func ResumeJob(ctx context.Context, h, ref string) (*Job, error) {
	return openCronStore(h, nil).resumeJob(ctx, ref)
}

// Schedules a job of the home for the next tick by id or name, nil when nothing matches
func TriggerJob(ctx context.Context, h, ref string) (*Job, error) {
	return openCronStore(h, nil).triggerJob(ctx, ref, "")
}

// Re-arms a completed one shot of the home at runAt, nil when nothing matches
func RearmOneshot(ctx context.Context, h, ref string, runAt any) (*Job, error) {
	return openCronStore(h, nil).rearmOneshot(ctx, ref, runAt)
}

// Current instant in the configured cron zone of the home as isoformat text
func NowISO(h string) string { return openCronStore(h, nil).clk.nowISO() }

// Operator facing state of a job, never paused while enabled and terminal states kept
func EffectiveJobState(job *Job) string { return effectiveJobState(job) }

// Writes the scheduled jobs of the home to w, every job when showAll
func CronList(ctx context.Context, h string, w io.Writer, showAll bool) error {
	return openCronStore(h, nil).cronList(ctx, w, showAll)
}

// Writes the cron scheduler status of the home to w
func CronStatus(ctx context.Context, h string, w io.Writer) error {
	return openCronStore(h, nil).cronStatus(ctx, w)
}
