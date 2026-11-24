package gateway

import "context"

// Directory system scope unit files live in
func SystemUnitDir() string { return systemUnitDir }

// Asks a gateway ancestor to restart itself through SIGUSR1, true when the signal went out
func RequestGatewaySelfRestart(pid int) bool { return requestGatewaySelfRestart(pid) }

// Bounded SIGTERM then SIGKILL of a gateway whose loop is proven dead, true once it is gone
func EscalateWedgedGateway(pid int) bool { return escalateWedgedGateway(pid, 5.0, 5.0) }

// Sends the drain aware restart signal and waits for the exit, false when unsent or alive at the timeout
func GracefulRestartViaSIGUSR1(pid int, drainTimeout float64, onProgress func()) bool {
	return gracefulRestartViaSIGUSR1(pid, drainTimeout, onProgress)
}

// Kickstart with kill of the label in the domain, a failed launchctl returned as the error
func LaunchdKickstart(ctx context.Context, label, domain string) error {
	return launchdKickstart(ctx, label, domain)
}

// Waits for the recorded gateway pid to exit without force killing it, true once gone
func WaitForGatewayExit(timeout float64) bool { return waitForGatewayExit(timeout, 0, false) }

// Rewrites the installed systemd unit when its definition is out of date, true when it was rewritten
func RefreshSystemdUnitIfNeeded(ctx context.Context, system bool) (bool, error) {
	return refreshSystemdUnitIfNeeded(ctx, system)
}
