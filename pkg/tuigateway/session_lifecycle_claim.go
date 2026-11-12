package tuigateway

// Refusal shown when a claim errors before it proved the session unowned
const sessionOwnershipUnavailable = "Icebath could not safely reserve this session. Try again."

// Writer identity of a lease, the live session half of the same writer check
func leaseMetadata(liveSessionID string) map[string]any {
	return map[string]any{"live_session_id": liveSessionID}
}
