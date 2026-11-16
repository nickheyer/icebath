package gatewayrun

import (
	"context"
	"net"
)

// Running gateway listener, its origin, serve task and bound socket
type gatewayAPIHandle struct {
	apiOrigin string
	listener  *sessionListener
	task      chan struct{}
	socket    net.Listener
	// Error the serve task ended with, set before task closes and nil for a clean exit
	serveErr error
}

// Asks the listener to end its sockets and waits for its serve task, never stopping the authority
func stopGatewayAPI(ctx context.Context, handle *gatewayAPIHandle) {
	handle.listener.requestExit()
	select {
	case <-handle.task:
	case <-ctx.Done():
	}
}
