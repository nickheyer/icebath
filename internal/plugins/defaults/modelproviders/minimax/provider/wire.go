package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// MiniMax M3 on the global OpenAI compatible address splits its reasoning out, an ask only choosing adaptive or no thinking
func shape(call *provider.Call) (map[string]any, map[string]any) {
	model := strings.ToLower(strings.TrimSpace(call.Model))
	if provider.BaseHost(call.Route.BaseURL) != "api.minimax.io" || provider.BasePath(call.Route.BaseURL) != "/v1" ||
		model != "minimax-m3" && model != "minimax/minimax-m3" {
		return nil, nil
	}
	extra := map[string]any{"reasoning_split": true}
	switch {
	case call.Ask.Set && call.Ask.Off:
		extra["thinking"] = map[string]any{"type": "disabled"}
	case call.Ask.Set:
		extra["thinking"] = map[string]any{"type": "adaptive"}
	}
	return extra, nil
}

// Prompt caching over the Anthropic Messages API, with blocks marked as Anthropic places them, except for MiniMax-M3, which refuses the
// markers; nothing decided over any other API
func caching(model string, mode provider.APIMode) (bool, bool, bool) {
	if mode != provider.ModeAnthropicMessages {
		return false, false, false
	}
	if strings.Contains(provider.BareModel(model), "minimax-m3") {
		return false, false, true
	}
	return true, true, true
}
