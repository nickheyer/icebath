package gatewayrun

// Whether an external supervisor started this gateway's drain
func (r *GatewayRunner) ExternalDrainActive() bool { return r.externalDrainActive }
