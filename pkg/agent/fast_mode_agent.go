package agent

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Agent slots of the runtime hooks other packages reach through the agent
type runtimeHookState struct {
	fastWindow         *provider.FastWindow
	daybreak           bool
	llmOutputTransform *llmOutputTransformRecord
}

// Session fast window carrying the live tier, overrides, window length and Daybreak opt in of the agent
func (a *AIAgent) fastMode() *provider.FastWindow {
	if a.fastWindow == nil {
		a.fastWindow = provider.NewFastWindow(a.serviceTier, a.requestOverrides)
	}
	f := a.fastWindow
	f.ServiceTier, f.RequestOverrides, f.AutoSeconds, f.Daybreak = a.serviceTier, a.requestOverrides, a.fastAutoSeconds, a.daybreak
	return f
}

// Route of the live agent the fast window and tier negotiation read
func (a *AIAgent) fastModeRoute() provider.Route {
	return provider.Route{Provider: a.provider, Model: a.model, BaseURL: a.baseURL, APIKey: a.apiKey, Mode: provider.APIMode(a.apiMode)}
}

// Opens or refuses the fast window at a user turn boundary, cold only without prior turns
func beginFastModeTurn(a *AIAgent, conversationHistory []map[string]any) {
	history := make([]*state.Message, 0, len(conversationHistory))
	for _, m := range conversationHistory {
		if role, ok := m["role"].(string); ok {
			history = append(history, &state.Message{Role: role})
		}
	}
	a.fastMode().BeginTurn(history)
}

// EffectiveRequestOverrides are the request overrides plus the fast override while the window is open
func (a *AIAgent) EffectiveRequestOverrides() map[string]any {
	return a.fastMode().EffectiveRequestOverrides(a.fastModeRoute(), a.anthropicBaseURL)
}

// Records that the live model has no fast capacity so later requests drop speed
func (a *AIAgent) markFastModeUnavailable() bool {
	return a.fastMode().MarkFastModeUnavailable(a.model)
}

// SetDaybreak turns the OpenAI Daybreak program on for the owner's local surfaces
func (a *AIAgent) SetDaybreak(on bool) { a.daybreak = on }

// ProviderHookFailures is the session set of failed provider profile hooks, emptied when the session resets
func (a *AIAgent) ProviderHookFailures() map[string]bool {
	if a.providerHookFailures == nil {
		a.providerHookFailures = map[string]bool{}
	}
	return a.providerHookFailures
}
