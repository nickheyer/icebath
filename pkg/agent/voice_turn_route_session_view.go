package agent

import "github.com/nickheyer/icebath/pkg/gateway"

// Route the agent reports as its session's own, the pre voice snapshot while a voice turn runs
func (a *AIAgent) SessionRuntime() gateway.SessionRuntimeFacts {
	if view := SessionRuntimeView(a); view != nil {
		return view
	}
	return nil
}
