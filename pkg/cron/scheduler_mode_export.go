package cron

// Built in ticker in place of an external provider once multiplexing, the provider unchanged otherwise
func SchedulerForProfileMode(h string, p CronScheduler, multiplexProfiles bool) CronScheduler {
	return openCronStore(h, nil).schedulerForProfileMode(p, multiplexProfiles)
}
