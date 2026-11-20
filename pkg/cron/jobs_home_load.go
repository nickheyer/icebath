package cron

import "context"

// Every job of the cron store under the home in load order, what the update drain report lists
func LoadJobsForHome(ctx context.Context, h string) ([]*Job, error) {
	return openCronStore(h, nil).LoadJobs(ctx)
}
