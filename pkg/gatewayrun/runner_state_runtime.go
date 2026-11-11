package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Attributes the session runtime lifecycle and the bootstrap set after the constructor
type runnerRuntimeState struct {
	sessionTicketStore   *gateway.TicketStore
	sessionControlServer *gateway.GatewayControlServer
	sessionAPI           *gatewayAPIHandle
}
