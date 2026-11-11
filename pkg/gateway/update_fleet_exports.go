package gateway

import "context"

// LegacyGatewayUnit is one pre rename gateway unit file on disk with the scope that holds it
type LegacyGatewayUnit struct {
	Name, Path string
	System     bool
}

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

// True when a pre rename gateway unit file exists in either scope
func HasLegacyHermesUnits(ctx context.Context) bool { return hasLegacyHermesUnits(ctx) }

// Pre rename gateway unit files on disk in discovery order
func FindLegacyHermesUnits(ctx context.Context) []LegacyGatewayUnit {
	found := findLegacyHermesUnits(ctx)
	out := make([]LegacyGatewayUnit, len(found))
	for i, unit := range found {
		out[i] = LegacyGatewayUnit{Name: unit.name, Path: unit.path, System: unit.system}
	}
	return out
}
