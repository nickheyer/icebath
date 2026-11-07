package agent

import "github.com/nickheyer/icebath/pkg/provider"

// Credits and rate limit capture the provider header captures feed for this agent
func (a *AIAgent) CreditsCapture() *provider.CreditsCapture { return a.creditsCapture() }
