package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Publishes the gateway state, exit reason, restart flag and active work, naming each unit only while draining
func (r *GatewayRunner) updateRuntimeStatus(ctx context.Context, gatewayState, exitReason *string) {
	if r.startupCleanupOnly {
		return
	}
	var activeWork any
	if gatewayState != nil && *gatewayState == "draining" {
		activeWork = r.describeActiveWork(ctx)
	}
	fields := map[string]any{"gateway_state": optAny(gatewayState), "exit_reason": optAny(exitReason),
		"restart_requested": r.restartRequested, "active_agents": r.activeWorkCount(), "active_work": activeWork}
	writeRuntimeStatusQuiet(ctx, gateway.RuntimeStatusUpdate{Fields: fields})
}
