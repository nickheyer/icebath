package gatewayrun

import "sync/atomic"

// Attributes the bootstrap and startup modules set lazily, after the constructor
type runnerStartupState struct {
	startupCleanupOnly     bool
	startupParkedPlatforms bool
	// A startup platform failure was a live foreign token or profile holder
	startupOwnershipConflict bool
	// Detached stop the runtime downgrade watcher started, held so the stop outlives the watcher
	runtimeDowngradeStop *gatewayTask
	// Flood redelivery timers the startup and reconnect paths arm, one per bot identity
	floodRedelivery floodRedelivery
	// Gateway health OTLP export the startup started, nil until then and after shutdown
	gatewayHealthExportRuntime gatewayHealthExport
	// Idle exit bookkeeping, the monotonic last client activity, attached clients and the stop it started
	idleExitLastActivity atomic.Uint64
	idleExitClients      atomic.Int64
	idleExitStopTask     *gatewayTask
}
