package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Anchor state view over the agent anchor fields and the session row the anchor mirrors to
func (a *AIAgent) usageAnchorState() *provider.UsageAnchorState {
	s := &provider.UsageAnchorState{Anchor: a.usageAnchor, TurnBaseAnchor: a.turnBaseUsageAnchor, PersistDisabled: a.persistDisabled, SessionID: a.sessionID}
	if a.sessionDB != nil {
		s.Store = a.sessionDB
	}
	return s
}

// Installs the anchor, nil clearing it and the turn base anchor, and mirrors it to the session row
func (a *AIAgent) setUsageAnchor(anchor *provider.UsageAnchor, turnBase bool) {
	s := a.usageAnchorState()
	s.Set(anchor, turnBase)
	a.usageAnchor, a.turnBaseUsageAnchor = s.Anchor, s.TurnBaseAnchor
}

// Adopts the persisted anchor while the resumed history keeps its priced prefix, else clears the stale blob
func restoreUsageAnchor(a *AIAgent, history []map[string]any) {
	s := a.usageAnchorState()
	s.Restore(rowsToMessages(history), history != nil)
	a.usageAnchor = s.Anchor
}
