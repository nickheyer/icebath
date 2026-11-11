package gateway

import "context"

// Dashboard web server entry points pkg/dashboard provides, assigned at startup by the command wiring
var WebServer struct {
	ShouldRequireDashboardAuth func(ctx context.Context, host string) (bool, error)
}

// Gate decision of the dashboard web server for the bind host, read through the wired entry point
func webServerRequiresAuth(ctx context.Context, host string) (bool, error) {
	return WebServer.ShouldRequireDashboardAuth(ctx, host)
}
