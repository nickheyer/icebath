package gatewayrun

// Degraded while a startup platform failure is parked, running otherwise
func (r *GatewayRunner) servingState() string {
	if r.startupParkedPlatforms {
		return "degraded"
	}
	return "running"
}
