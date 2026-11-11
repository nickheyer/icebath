package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Private bootstrap ticket store of the session runtime, nil before the runtime starts
func (r *GatewayRunner) SessionTicketStore() *gateway.TicketStore { return r.sessionTicketStore }
