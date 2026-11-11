package gatewayrun

import (
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Attributes the session runtime lifecycle and the bootstrap set after the constructor
type runnerRuntimeState struct {
	sessionTicketStore   *gateway.TicketStore
	sessionControlServer *gateway.GatewayControlServer
	sessionAPI           *gatewayAPIHandle
	// Webapp listener status the webapp control verbs answer with, replaced whole on every change
	webappStatus atomic.Pointer[webappListenerStatus]
	// Background dashboard mount started once the gateway is ready
	gatewayDashboardWarmup *gatewayTask
}
