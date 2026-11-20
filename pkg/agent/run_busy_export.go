package agent

// Running child agents snapshotted under their lock
func (a *AIAgent) ActiveChildren() []*AIAgent { return a.activeChildrenSnapshot() }

// Whether the agent can redirect its active turn onto a new message
func (a *AIAgent) SupportsActiveTurnRedirect() bool { return a.supportsActiveTurnRedirect }
