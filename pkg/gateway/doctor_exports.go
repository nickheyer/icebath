package gateway

// Base name of the gateway service unit every profile suffix extends
const ServiceBase = serviceBase

// The one live host gateway and its served profiles, nil when no gateway owns the role
func CurrentHostGatewayTopology() (*HostGatewayTopology, error) { return hostGatewayTopology() }
