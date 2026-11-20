package cron

// Replaces the write probe of the cron store and returns the one it replaced
func SwapStoreProbe(probe func(cronDir string) error) func(cronDir string) error {
	previous := storeHealthHooks.probe
	storeHealthHooks.probe = probe
	return previous
}
