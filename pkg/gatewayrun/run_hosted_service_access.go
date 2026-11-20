package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Private control listener the hosted room transport registers its attest and producer verbs on
func (r *GatewayRunner) SessionControlServer() *gateway.GatewayControlServer {
	return r.sessionControlServer
}
