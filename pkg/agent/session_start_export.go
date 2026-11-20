package agent

import "time"

// Wall clock time the agent's session started
func (a *AIAgent) SessionStart() time.Time { return a.sessionStart }
