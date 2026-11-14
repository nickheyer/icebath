package cron

import "context"

// Store operations the external distribution tests drive, exported only to the test build
type ExportedStore struct{ s *Store }

func ExportOpenStore(home string) ExportedStore { return ExportedStore{openCronStore(home, nil)} }

func (e ExportedStore) CreateJob(promptText, schedule, name string) (*Job, error) {
	return e.s.createJob(context.Background(), CreateJobOptions{Prompt: &promptText, Schedule: schedule, Name: &name})
}

func (e ExportedStore) ListJobs(includeDisabled bool) ([]*Job, error) {
	return e.s.listJobs(context.Background(), includeDisabled)
}

func (e ExportedStore) UpdateJob(id string, updates map[string]any) (*Job, error) {
	return e.s.updateJob(context.Background(), id, updates)
}

func (e ExportedStore) PauseJob(id string, reason *string) (*Job, error) {
	return e.s.pauseJob(context.Background(), id, reason)
}

func (e ExportedStore) ResumeJob(id string) (*Job, error) {
	return e.s.resumeJob(context.Background(), id)
}

func (e ExportedStore) DueJobs() ([]*Job, error) { return e.s.getDueJobs(context.Background()) }

func ExportIsJobRunnable(job *Job) bool { return isJobRunnable(job) }
