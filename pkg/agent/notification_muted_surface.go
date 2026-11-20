package agent

import "context"

// Context whose presentation events of the session are muted, the muted surface of a diagnostic only turn
func WithMutedSurface(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, mutedSurfaceKey{}, sessionID)
}
