package provider

import (
	"maps"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Efforts of Moonshot's K3 models, with the level a missing one maps to
var (
	k3Efforts   = []string{"low", "high", "max"}
	k3Overrides = map[string]string{"medium": "high", "xhigh": "max"}
)

// Host of Kimi's coding endpoint, which takes requests from coding agents
const codingHost = "api.kimi.com"

// Effort when one is asked, else the thinking toggle; K2 thinking keeps the replayed reasoning
func shape(call *provider.Call) (map[string]any, map[string]any) {
	extra, top := provider.ThinkingToggle(call.Ask, k3Efforts, k3Overrides, false)
	if thinking, ok := extra["thinking"].(map[string]any); ok && thinking["type"] == "enabled" && strings.Contains(strings.ToLower(call.Model), "k2") {
		thinking = maps.Clone(thinking)
		thinking["keep"] = "all"
		extra["thinking"] = thinking
	}
	return extra, top
}

// User agent Kimi's coding endpoint takes
func headers(call *provider.Call) map[string]string {
	if provider.BaseHostMatches(call.Route.BaseURL, codingHost) {
		return map[string]string{"User-Agent": "claude-code/0.1.0"}
	}
	return nil
}

// Prefix of the keys of Kimi's coding plan, which only its coding endpoint takes
const codingKey = "sk-kimi-"

// Address of Kimi's coding endpoint, which speaks the Anthropic Messages API
const codingEndpoint = "https://api.kimi.com/coding"

// A coding plan key goes to the coding endpoint over the Anthropic Messages API, unless the provider is set to an address of its own
func codingRoute(defaultBase string) func(provider.Route, config.View) provider.Route {
	return func(r provider.Route, _ config.View) provider.Route {
		if strings.HasPrefix(strings.TrimSpace(r.APIKey), codingKey) && strings.TrimRight(r.BaseURL, "/") == strings.TrimRight(defaultBase, "/") {
			r.BaseURL, r.Mode = codingEndpoint, provider.ModeAnthropicMessages
		}
		return r
	}
}
