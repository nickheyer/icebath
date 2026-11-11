package tuigateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
)

// Refusal shown when a claim errors before it proved the session unowned
const sessionOwnershipUnavailable = "Icebath could not safely reserve this session. Try again."

// Writer identity of a lease, the live session half of the same writer check
func leaseMetadata(liveSessionID string) map[string]any {
	return map[string]any{"live_session_id": liveSessionID}
}

// ClaimActiveSessionSlot claims the session's active lease for a surface, failing closed with a refusal when the claim errors
func ClaimActiveSessionSlot(ctx context.Context, sessionKey, liveSessionID, surface, profileHome string) (*activesessions.Lease, *string) {
	lease, refusal, err := activesessions.TryAcquire(ctx, sessionKey, surface, loadCfg(ctx), leaseMetadata(liveSessionID), profileHome,
		strings.ToLower(strings.TrimSpace(surface)) == "desktop")
	if err != nil {
		slog.Default().With("logger", "tui_gateway.server").Warn(fmt.Sprintf("Failed to claim active session slot: %s", err))
		unavailable := sessionOwnershipUnavailable
		return nil, &unavailable
	}
	if refusal != nil {
		text := refusal.PyStr()
		return lease, &text
	}
	return lease, nil
}
