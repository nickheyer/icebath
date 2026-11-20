package kanban

// Terminal timeout a bound worker exports for its run budget, nil when the current one stands
func WorkerTerminalTimeoutEnv(maxRuntimeSeconds *int64, currentTimeout *string) *string {
	return workerTerminalTimeoutEnv(maxRuntimeSeconds, currentTimeout)
}

// Start fingerprint of a live pid, nil when it cannot be read
func ProcessFingerprint(pid int) *string { return processFingerprint(pid) }

// True when the pid is live and still the worker fingerprinted at spawn
func WorkerAlive(pid, startedAt any) bool { return workerAlive(pid, startedAt) }
