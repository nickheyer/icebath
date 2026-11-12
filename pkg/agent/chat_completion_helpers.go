package agent

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Credits and rate limit capture the provider header captures feed for this agent
func (a *AIAgent) CreditsCapture() *provider.CreditsCapture { return a.creditsCapture() }

// Implicit stale timeout capped at half the remaining run budget, never under sixty seconds
func capToRunBudget(a *AIAgent, timeout float64) float64 {
	return provider.CapToRunBudget(a, timeout)
}

var _ provider.RunBudgetAgent = (*AIAgent)(nil)

// Wall clock run budget in seconds and the epoch second the turn clock started, zero when unset
func (a *AIAgent) RunBudget() (float64, float64) { return a.runBudgetSeconds, a.runBudgetStartedAt }

// Codex silence floor when the reasoning config is enabled at high effort or stronger, else zero
func highEffortSilenceFloor(a *AIAgent) float64 {
	cfg := a.reasoningConfig
	if cfg == nil {
		return 0
	}
	if enabled, ok := cfg["enabled"].(bool); ok && !enabled {
		return 0
	}
	effort := ""
	if v := cfg["effort"]; pyTruthy(v) {
		effort = strings.ToLower(strings.TrimSpace(pyStrValue(v)))
	}
	idx := provider.EffortLadderIndex(effort)
	if idx < 0 || idx < provider.EffortLadderIndex("high") {
		return 0
	}
	return provider.HighEffortSilenceFloorSeconds
}
