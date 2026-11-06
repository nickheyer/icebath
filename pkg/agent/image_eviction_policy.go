package agent

// Retired outbound image call ids latched for the session that retired them
type retiredImageIDs struct {
	sessionID string
	ids       map[string]bool
}

type imageEvictionState struct {
	outboundRetiredImageIDsHeld *retiredImageIDs
}

// Session latched set of tool call ids whose images the send path retired, fresh for a new session
func outboundRetiredImageIDs(a *AIAgent) map[string]bool {
	held := a.outboundRetiredImageIDsHeld
	if held == nil || held.sessionID != a.sessionID {
		held = &retiredImageIDs{sessionID: a.sessionID, ids: map[string]bool{}}
		a.outboundRetiredImageIDsHeld = held
	}
	return held.ids
}
