package backend

// Running version a gateway reports on /api/health, displayVersion first, empty when the gateway is offline
func ResolveGatewayVersion(request func(path string) (any, error)) string {
	status, err := request("/api/health")
	if err != nil {
		return ""
	}
	fields, ok := status.(map[string]any)
	if !ok {
		return ""
	}
	if display, ok := fields["displayVersion"].(string); ok && display != "" {
		return display
	}
	if version, ok := fields["version"].(string); ok {
		return version
	}
	return ""
}
