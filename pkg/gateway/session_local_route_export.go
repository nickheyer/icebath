package gateway

import "context"

// Launch params with an explicit model decoded for callers outside the package, the resume policy check of the CLI
func ResolveLaunchRoute(ctx context.Context, params, cfg map[string]any) (map[string]any, error) {
	resolved, _, err := resolveLaunchRoute(ctx, params, cfg)
	return resolved, err
}
