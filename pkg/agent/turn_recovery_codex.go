package agent

// Starts a new route with Codex reasoning replay on, the replay verdict belonging to the route that earned it
func resetCodexReasoningReplay(a *AIAgent) {
	a.codexReasoningReplayEnabled = true
	a.codexReasoningReplayRejected = false
}
