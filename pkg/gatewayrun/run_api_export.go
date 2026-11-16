package gatewayrun

import (
	"context"
	"net"
)

// Running gateway listener with its origin, serve task and socket
type GatewayAPIHandle = gatewayAPIHandle

// Binds the loopback gateway listener serving the native session socket
func StartGatewayAPI(ctx context.Context, r *GatewayRunner) (*GatewayAPIHandle, error) {
	return startGatewayAPI(ctx, r)
}

// Ends the listener sockets and waits for its serve task, never stopping the authority
func StopGatewayAPI(ctx context.Context, handle *GatewayAPIHandle) { stopGatewayAPI(ctx, handle) }

// Origin the listener answers on
func (h *gatewayAPIHandle) APIOrigin() string { return h.apiOrigin }

// Bound socket the listener accepts on
func (h *gatewayAPIHandle) Socket() net.Listener { return h.socket }

// Records the listener as the session API of the runner
func (r *GatewayRunner) SetSessionAPI(handle *GatewayAPIHandle) { r.sessionAPI = handle }
