package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Routes the provider summary path hooks to the agent sanitizers for an AIAgent caller
func init() {
	provider.SanitizeOutboundKwargsHook = func(agent any, apiKwargs map[string]any) {
		agent.(*AIAgent).sanitizeOutboundKwargs(apiKwargs)
	}
	provider.StripImagesForRejectingModelHook = func(agent any, apiMessages *[]any) bool {
		return agent.(*AIAgent).stripImagesForRejectingModel(apiMessages)
	}
}
