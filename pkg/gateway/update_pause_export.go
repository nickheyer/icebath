package gateway

import (
	"context"
	"io"
	"time"
)

// Points the user systemd environment at this account for the update pause
func EnsureUserSystemdEnv() { ensureUserSystemdEnv() }

// Live argv of a running gateway for the update pause, nil when it is not a gateway
func CaptureGatewayArgv(pid int) []string { return captureGatewayArgv(pid) }

// True when target is this process or one of its ancestors
func IsPidAncestorOfCurrentProcess(target int) bool { return isPidAncestorOfCurrentProcess(target) }

// Bootstraps a launchd job, booting a stale registration out once before the retry
func LaunchctlBootstrap(ctx context.Context, domain, plistPath, label string, timeout time.Duration) error {
	return launchctlBootstrap(ctx, domain, plistPath, label, timeout)
}

// True once the label runs a pid other than the old one within the timeout
func WaitForLaunchdServicePID(ctx context.Context, label string, oldPid int, haveOld bool, timeout float64, domain string) (bool, error) {
	return waitForLaunchdServicePID(ctx, label, oldPid, haveOld, timeout, domain)
}

// Seconds a restart may drain in flight turns, from the environment override else the agent config
func GetRestartDrainTimeout(ctx context.Context) (float64, error) { return getRestartDrainTimeout(ctx) }

// Whether pid is still the icebath process born at the expected start time
func PidIsHermes(pid int, expectedStartTime any) bool { return pidIsHermes(pid, expectedStartTime) }

// Respawns stopped dashboard and serve backends from their commands, the commands that could not start
func RespawnDashboardProcesses(ctx context.Context, w io.Writer, commands [][]string) [][]string {
	return respawnDashboardProcesses(ctx, w, commands)
}

// Records the gateway pids a start vouched for so a later death is reported
func WriteStartAttestation(ctx context.Context, pids []int, via string) {
	writeStartAttestation(ctx, pids, via)
}

// Rewrites the installed Windows gateway launcher scripts in place, the script path
func WriteWindowsTaskScript(ctx context.Context) (string, error) { return writeWindowsTaskScript(ctx) }

// Whether the live command line of pid is a gateway run
func LooksLikeGatewayProcess(pid int) bool { return looksLikeGatewayProcessLive(pid) }
