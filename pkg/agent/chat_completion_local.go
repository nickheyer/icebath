package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Lets one attempt past the open stale breaker when the agent's llama-server answers its health check again
func retryRecoveredLocalServer(a *AIAgent) bool {
	return a.RetrySession().RetryRecoveredLocalServer(provider.APIMode(a.apiMode), a.provider, a.baseURL)
}
