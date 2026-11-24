package kanban

import "context"

// Terminal timeouts a bound worker exports for its run budget over what the profile of ctx allows, by env name
func WorkerTerminalTimeouts(ctx context.Context, maxRuntimeSeconds *int64) map[string]string {
	return workerTerminalTimeouts(ctx, maxRuntimeSeconds)
}

// Start fingerprint of a live pid, nil when it cannot be read
func ProcessFingerprint(pid int) *string { return processFingerprint(pid) }

// True when the pid is live and still the worker fingerprinted at spawn
func WorkerAlive(pid, startedAt any) bool { return workerAlive(pid, startedAt) }
