package provider

// Profiles the bundled gmi plugin registers
func gmiPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "gmi", display: "GMI Cloud", desc: "GMI Cloud — multi-model direct API (slash-form model IDs)",
		signup: "https://www.gmicloud.ai/", keys: []string{"GMI_API_KEY", "GMI_BASE_URL"}, base: "https://api.gmi-serving.com/v1",
		authType: AuthTypeAPIKey, fallbacks: []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5",
			"google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
	}, "gmi-cloud", "gmicloud")
	p.LazyHeaders = NewLazyHeaders(HeaderEntry{Key: "User-Agent", Fn: func() string { return UserAgent("IcebathAgent") }})
	p.DefaultAuxModel = "google/gemini-3.1-flash-lite-preview"
	return []Profile{p}
}

// Profiles the bundled novita plugin registers
func novitaPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "novita", display: "NovitaAI", desc: "NovitaAI — AI-native cloud for builders and agents",
		signup: "https://novita.ai/settings/key-management", keys: []string{"NOVITA_API_KEY", "NOVITA_BASE_URL"},
		base: "https://api.novita.ai/openai/v1", authType: AuthTypeAPIKey, fallbacks: []string{"moonshotai/kimi-k2.5", "minimax/minimax-m2.7",
			"zai-org/glm-5", "deepseek/deepseek-v3-0324", "deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
	}, "novita-ai", "novitaai")
	p.DefaultAuxModel = "deepseek/deepseek-v3-0324"
	return []Profile{p}
}
