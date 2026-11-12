package gatewayrun

import (
	"context"
	"net"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Running gateway API listener with its origin, server, serve task, dashboard mount and socket
type GatewayAPIHandle = gatewayAPIHandle

// Binds the gateway listener on host and port serving the native session socket and the lazily mounted dashboard
func StartGatewayAPI(ctx context.Context, r *GatewayRunner, host string, port int, webapp *gateway.WebappMount) (*GatewayAPIHandle, error) {
	return startGatewayAPI(ctx, r, host, port, webapp)
}

// Drains the listener sockets and waits for its serve task, never stopping the authority
func StopGatewayAPI(ctx context.Context, handle *GatewayAPIHandle) { stopGatewayAPI(ctx, handle) }

// Origin the listener answers on
func (h *gatewayAPIHandle) APIOrigin() string { return h.apiOrigin }

// Bound socket the listener accepts on
func (h *gatewayAPIHandle) Socket() net.Listener { return h.socket }

// Dashboard mount the listener serves under the runtime API
func (h *gatewayAPIHandle) App() gateway.GatewayDashboardMount { return h.app }

// Records the listener as the session API of the runner, the session_api attribute
func (r *GatewayRunner) SetSessionAPI(handle *GatewayAPIHandle) { r.sessionAPI = handle }
