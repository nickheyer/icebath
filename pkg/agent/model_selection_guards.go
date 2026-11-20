package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Selection guard context of a live agent from its last billed prompt, nil without a positive measurement
func SelectionContextForAgent(a *AIAgent) *provider.SelectionContext {
	if a == nil || a.ContextCompressor() == nil {
		return nil
	}
	return provider.SelectionContextFor(a.ContextCompressor().Base().LastPromptTokens, a.Model())
}
