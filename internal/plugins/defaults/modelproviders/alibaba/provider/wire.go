package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Asks DashScope's compatible mode to keep the reasoning of the turns replayed to the model
func shape(call *provider.Call) (map[string]any, map[string]any) {
	return provider.DashScopeThinking(call.Model), nil
}

// Prompt caching of Qwen models on the message shape, which DashScope keeps for five minutes at most
func caching(model string, _ provider.APIMode) (bool, bool, bool) {
	if strings.Contains(strings.ToLower(model), "qwen") {
		return true, false, true
	}
	return false, false, false
}

func cacheTTL(string) string { return "5m" }
