package gateway

import "context"

// True when launchd refused this domain and the gateway runs on the detached fallback
func LaunchdUnsupportedMarkerExists(ctx context.Context) bool { return launchdUnsupportedMarkerExists(ctx) }
