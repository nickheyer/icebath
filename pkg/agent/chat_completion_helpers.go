package agent

import (
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Credits and rate limit capture the provider header captures feed for this agent
func (a *AIAgent) CreditsCapture() *provider.CreditsCapture { return a.creditsCapture() }

// Implicit stale timeout capped at half the remaining run budget, never under sixty seconds
func capToRunBudget(a *AIAgent, timeout float64) float64 {
	if a.runBudgetSeconds == 0 || a.runBudgetStartedAt == 0 {
		return timeout
	}
	remaining := a.runBudgetSeconds - (nowSeconds() - a.runBudgetStartedAt)
	return math.Min(timeout, math.Max(60.0, remaining*0.5))
}

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
