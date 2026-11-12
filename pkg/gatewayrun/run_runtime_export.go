package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Builds the authorities of the reserved served homes and their runtime descriptor, the initialize_gateway_runtime step
func InitializeGatewayRuntime(ctx context.Context, r *GatewayRunner) error {
	return r.initializeGatewayRuntime(ctx)
}

// Starts the session API listener and records its origin in the runtime descriptor
func StartGatewayRuntimeAPI(ctx context.Context, r *GatewayRunner) error {
	return startGatewayRuntimeAPI(ctx, r)
}

// Marks the runtime ready with its capabilities, refused once the runner stopped or its listener ended
func PublishGatewayRuntimeReady(r *GatewayRunner) error { return publishGatewayRuntimeReady(r) }

// Origin the session API listener answers on, read through the started listener
func (r *GatewayRunner) SessionAPIOrigin() string { return r.sessionAPI.apiOrigin }

// Binds the control server the runtime registers its verbs on, the session_control_server attribute
func (r *GatewayRunner) SetSessionControlServer(server *gateway.GatewayControlServer) {
	r.sessionControlServer = server
}

// Binds the running loop the runner schedules its loop work on, the loop start reads as get_running_loop
func (r *GatewayRunner) BindGatewayLoop(loop gateway.EventLoop) {
	r.gatewayLoop = loop
}
