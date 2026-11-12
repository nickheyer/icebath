package gateway

// Read form of a split cron argument, overview, list or list-all, empty for a write
func CronRead(tokens []string) string { return cronRead(tokens) }
