package provider

import "strings"

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
	parts, err := pyURLParse(strings.TrimSpace(baseURL), "")
	if err != nil {
		return false
	}
	host, _, _ := pyNetlocHostPort(parts.netloc)
	return host == "api.minimax.io" && strings.ToLower(strings.TrimRight(parts.path, "/")) == "/v1"
}

// M3 on the OpenAI compatible route splits reasoning out, efforts only choosing adaptive or disabled thinking
func minimaxBuildAPIKwargsExtras(reasoning *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	model := strings.ToLower(strings.TrimSpace(hookString(kw, "model")))
	if !minimaxGlobalOpenAIBaseURL(hookString(kw, "base_url")) || (model != "minimax-m3" && model != "minimax/minimax-m3") {
		return map[string]any{}, map[string]any{}
	}
	extra := map[string]any{"reasoning_split": true}
	if reasoning != nil && reasoning.Enabled != nil && !*reasoning.Enabled {
		extra["thinking"] = map[string]any{"type": "disabled"}
	} else if reasoning != nil {
		extra["thinking"] = map[string]any{"type": "adaptive"}
	}
	return extra, map[string]any{}
}
