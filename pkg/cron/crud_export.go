package cron

// Whether the scheduler may fire the job, enabled and free of pause markers
func IsJobRunnable(job *Job) bool { return isJobRunnable(job) }

// Whether the record sits in a terminal scheduler state
func IsTerminalJob(job *Job) bool { return isTerminalJob(job) }
