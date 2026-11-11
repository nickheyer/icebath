package cron

// Snapshot of the executing job ids the gateway drain and work counts read
func GetRunningJobIDs() map[string]bool { return getRunningJobIDs() }
