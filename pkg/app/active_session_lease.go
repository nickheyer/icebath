package app

import (
	"context"

	"github.com/nickheyer/icebath/pkg/activesessions"
)

// Claims a slot in the shared active session registry, a refusal carrying its machine readable reason
func AcquireActiveSessionLease(ctx context.Context, sessionID, surface string, cfg map[string]any, metadata map[string]any) (ActiveSessionLease, *activesessions.Refusal, error) {
	lease, refusal, err := activesessions.TryAcquire(ctx, sessionID, surface, cfg, metadata, "", false)
	if lease == nil {
		return nil, refusal, err
	}
	return lease, refusal, err
}
