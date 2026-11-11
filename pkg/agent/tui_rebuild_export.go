package agent

// Whether this agent holds a dedicated session db handle it must close
func (a *AIAgent) OwnsSessionDB() bool { return a.ownsSessionDB }

// Adds raw spawning task ids inherited from a replaced agent to the process owner set
func (a *AIAgent) AdoptProcessOwnerTaskIDs(ids []string) {
	if a == nil || len(ids) == 0 {
		return
	}
	a.processOwnerMu.Lock()
	defer a.processOwnerMu.Unlock()
	if a.processOwnerTaskIDs == nil {
		a.processOwnerTaskIDs = map[string]bool{}
	}
	for _, id := range ids {
		a.processOwnerTaskIDs[id] = true
	}
}

// Replaces the ephemeral system prompt appended at API call time, empty clearing it
func (a *AIAgent) SetEphemeralSystemPrompt(prompt string) { a.ephemeralSystemPrompt = prompt }
