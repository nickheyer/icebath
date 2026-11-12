package tuigateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
)

// Claims the active lease for a surface, a reasonless ownership refusal when the claim itself errors
func ClaimActiveSessionSlotRefusal(ctx context.Context, sessionKey, liveSessionID, surface, profileHome string) (*activesessions.Lease, *activesessions.Refusal) {
	lease, refusal, err := activesessions.TryAcquire(ctx, sessionKey, surface, loadCfg(ctx), leaseMetadata(liveSessionID), profileHome,
		strings.ToLower(strings.TrimSpace(surface)) == "desktop")
	if err != nil {
		slog.Default().With("logger", "tui_gateway.server").Warn(fmt.Sprintf("Failed to claim active session slot: %s", err))
		return nil, &activesessions.Refusal{Message: sessionOwnershipUnavailable}
	}
	return lease, refusal
}

// Writer identity of a lease for one live session id
func LeaseMetadata(liveSessionID string) map[string]any { return leaseMetadata(liveSessionID) }

// Refusal text shown when a claim errors before it proved the session unowned
const SessionOwnershipUnavailable = sessionOwnershipUnavailable
