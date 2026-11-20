package gateway

import "context"

// Commits an automation wake of a route to its session authority FIFO, false when no authority owns the route
func AdmitRouteWake(ctx context.Context, runner AuthorityRunner, adapter PlatformAdapter, source *SessionSource, text, kind string) (bool, error) {
	return admitRouteWake(ctx, runner, adapter, source, text, kind, nil)
}
