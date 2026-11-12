package gateway

import "context"

// Dashboard web server entry points pkg/dashboard provides, assigned at startup by the command wiring
var WebServer struct {
	ShouldRequireDashboardAuth func(ctx context.Context, host string) (bool, error)
	StartServer                func(ctx context.Context, o DashboardServerOptions) error
}

// Gate decision of the dashboard web server for the bind host, read through the wired entry point
func webServerRequiresAuth(ctx context.Context, host string) (bool, error) {
	return WebServer.ShouldRequireDashboardAuth(ctx, host)
}

// Runs the dashboard web server through the wired entry point until it stops
func startDashboardServer(ctx context.Context, o DashboardServerOptions) error {
	return WebServer.StartServer(ctx, o)
}
