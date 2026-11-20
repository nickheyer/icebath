package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Session rejected thinking tracker rebound to the live route and shared with the context compressor
func (a *AIAgent) rejectedThinkingTracker() *provider.RejectedThinkingTracker {
	t := a.rejectedThinking
	if t == nil {
		t = &provider.RejectedThinkingTracker{}
		a.rejectedThinking = t
	}
	t.Mode, t.BaseURL, t.Model = provider.APIMode(a.apiMode), a.baseURL, a.model
	t.SessionID, t.PersistDisabled = a.sessionID, a.persistDisabled
	t.Store = nil
	if store, ok := a.sessionDB.(provider.RejectedThinkingStore); ok {
		t.Store = store
	}
	t.ResetUsageAnchor = func() { a.setUsageAnchor(nil, false) }
	if c, ok := a.contextCompressor.(*ContextCompressor); ok && c != nil {
		c.SetRejectedThinkingTracker(t)
	}
	return t
}

// Filters the session's rejected thinking blocks from a request copy rebuilt from canonical history
func applyRejectedThinkingSuppression(a *AIAgent, messages []map[string]any) int {
	return a.rejectedThinkingTracker().ApplyRejectedThinkingSuppression(messages)
}

// Fingerprints the rejected request and strips its retry copy without mutating history
func rememberRejectedThinking(a *AIAgent, apiMessages []map[string]any) int {
	return a.rejectedThinkingTracker().RememberRejectedThinking(apiMessages)
}

// Carries the old session's rejected fingerprints onto the compression continuation session
func carryRejectedThinkingToSession(a *AIAgent, oldSessionID string) {
	a.rejectedThinkingTracker().CarryRejectedThinkingToSession(oldSessionID)
}
