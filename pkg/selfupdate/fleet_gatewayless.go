package selfupdate

import (
	"context"
	"path/filepath"
)

var supervisedServeBackends = map[string]bool{"manual-serve": true, "desktop": true, "desktop-ssh": true, "systemd": true, "launchd": true,
	"windows-service": true, "service": true}

// Serve backends whose supervisor restarts the process with no updater bookkeeping
var supervisorOwnedServeBackends = map[string]bool{"desktop": true, "desktop-ssh": true, "systemd": true, "launchd": true, "windows-service": true,
	"service": true}

// True for a serve or dashboard row the gateway matrix neither covers nor needs to
func runtimeOutsideGatewayEvidence(ctx context.Context, runtime any) bool {
	if !isServeKind(runtime) {
		return false
	}
	if DeferManualServe(ctx, runtime, false) {
		return true
	}
	supervisor, isStr := rowValue(runtime, "supervisor").(string)
	return isStr && supervisorOwnedServeBackends[supervisor]
}

var gatewaylessInventory = CollectRuntimeInventory

// True when no profile expects a running gateway and every live runtime is outside the gateway matrix
func hostOwesNoGatewayRestart(ctx context.Context) bool {
	for _, ph := range fleetHomes() {
		record := fleetReadStatus(filepath.Join(ph.home, "gateway_state.json"))
		if record == nil {
			continue
		}
		state, isStr := rowValue(record, "gateway_state").(string)
		if !isStr || !notExpectedStates[state] {
			return false
		}
	}
	for _, runtime := range gatewaylessInventory(ctx).Runtimes {
		if !runtimeOutsideGatewayEvidence(ctx, runtime.ToDict()) {
			return false
		}
	}
	return true
}
