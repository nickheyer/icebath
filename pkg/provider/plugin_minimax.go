package provider

import (
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

// Profiles the bundled minimax plugin registers
func minimaxPluginProfiles() []Profile {
	global := bundledRowProfile(profileRow{name: "minimax", mode: ModeAnthropicMessages, keys: []string{"MINIMAX_API_KEY"},
		base: "https://api.minimax.io/anthropic", authType: AuthTypeAPIKey}, "mini-max")
	global.DefaultAuxModel = "MiniMax-M3"
	cn := bundledRowProfile(profileRow{name: "minimax-cn", mode: ModeAnthropicMessages, keys: []string{"MINIMAX_CN_API_KEY"},
		base: "https://api.minimaxi.com/anthropic", authType: AuthTypeAPIKey}, "minimax-china", "minimax_cn")
	cn.DefaultAuxModel = "MiniMax-M3"
	oauth := bundledRowProfile(profileRow{name: "minimax-oauth", mode: ModeAnthropicMessages, display: "MiniMax (OAuth)",
		desc: "MiniMax via OAuth browser flow — no API key required", signup: "https://api.minimax.io/",
		base: "https://api.minimax.io/anthropic", authType: AuthTypeOAuthExternal},
		"minimax_oauth", "minimax-portal", "minimax-global", "minimax-oauth-io")
	oauth.DefaultAuxModel = "MiniMax-M2.7"
	out := []Profile{global, cn, oauth}
	for i := range out {
		out[i].BuildAPIKwargsExtrasHook = minimaxBuildAPIKwargsExtras
	}
	return out
}

// True for the global OpenAI compatible api.minimax.io/v1 route
func minimaxGlobalOpenAIBaseURL(baseURL string) bool {
	parts, err := urlutil.Split(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	host, _, _ := netlocHostPort(parts.Authority)
	return host == "api.minimax.io" && strings.ToLower(strings.TrimRight(parts.Path, "/")) == "/v1"
}

// M3 on the OpenAI compatible route splits reasoning out, efforts only choosing adaptive or disabled thinking
func minimaxBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (*jsonutil.Object, map[string]any) {
	model := strings.ToLower(strings.TrimSpace(hookString(kw, "model")))
	if !minimaxGlobalOpenAIBaseURL(hookString(kw, "base_url")) || (model != "minimax-m3" && model != "minimax/minimax-m3") {
		return jsonutil.ObjectOf(), map[string]any{}
	}
	extra := jsonutil.ObjectOf("reasoning_split", true)
	if reasoning != nil && reasoning.Enabled != nil && !*reasoning.Enabled {
		extra.Set("thinking", jsonutil.ObjectOf("type", "disabled"))
	} else if reasoning != nil {
		extra.Set("thinking", jsonutil.ObjectOf("type", "adaptive"))
	}
	return extra, map[string]any{}
}
