package cron

// PersistedErrorRecoveryStats is the probe snapshot of stale error re-arms
func PersistedErrorRecoveryStats() map[string]any {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	return map[string]any{"persisted_error_recoveries": persistedErrorRecoveries,
		"recent": append([]map[string]any{}, persistedErrorRecoveriesRecent...)}
}

// TimezoneMigrationCatchupStats is the probe snapshot of offset migration catch up fires
func TimezoneMigrationCatchupStats() map[string]any {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	return map[string]any{"timezone_migration_catchups": timezoneMigrationCatchups,
		"recent": append([]map[string]any{}, timezoneMigrationCatchupsRecent...)}
}
