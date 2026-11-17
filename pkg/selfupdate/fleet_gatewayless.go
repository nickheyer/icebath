package selfupdate

import (
	"context"
	"path/filepath"
)

var gatewaylessInventory = CollectRuntimeInventory

// True when no profile expects a running gateway and no runtime is live
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
	return len(gatewaylessInventory(ctx).Runtimes) == 0
}
