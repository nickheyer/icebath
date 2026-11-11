package gatewayrun

import (
	"context"
	"net"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Running gateway API listener, its origin, server, dashboard mount, socket and the task serving it
type gatewayAPIHandle struct {
	apiOrigin string
	server    gateway.GatewayListenerServer
	task      chan struct{}
	app       gateway.GatewayDashboardMount
	socket    net.Listener
	// Error the serve task ended with, set before task closes and nil for a clean exit
	serveErr error
}

// Asks the listener to drain its sockets and waits for its serve task, never stopping the authority
func stopGatewayAPI(ctx context.Context, handle *gatewayAPIHandle) {
	handle.server.RequestExit()
	select {
	case <-handle.task:
	case <-ctx.Done():
	}
}
