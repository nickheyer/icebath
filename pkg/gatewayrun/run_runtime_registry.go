package gatewayrun

import "github.com/nickheyer/icebath/pkg/gateway"

// Registry of every profile authority the runtime serves, nil before the runtime initializes
func (r *GatewayRunner) SessionAuthorities() *gateway.SessionAuthorities {
	return r.sessionAuthorities
}

var _ gateway.AuthorityRegistryRunner = (*GatewayRunner)(nil)
