package provider

import "github.com/nickheyer/icebath/internal/provider"

// Asks DashScope's compatible mode to keep the reasoning of the turns replayed to the model
func shape(call *provider.Call) (map[string]any, map[string]any) {
	return provider.DashScopeThinking(call.Model), nil
}
