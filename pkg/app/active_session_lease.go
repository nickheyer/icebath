package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Claims a slot in the shared active session registry under the session limit setting of ctx, a refusal carrying its machine readable reason
func AcquireActiveSessionLease(ctx context.Context, sessionID, surface string, metadata map[string]any) (ActiveSessionLease, *activesessions.Refusal, error) {
	var limit any
	if n, set := settings.MaxConcurrentSessions.Lookup(ctx); set {
		limit = n
	}
	lease, refusal, err := activesessions.TryAcquire(ctx, sessionID, surface, map[string]any{settings.MaxConcurrentSessions.Key: limit}, metadata, "", false)
	if lease == nil {
		return nil, refusal, err
	}
	return lease, refusal, err
}
