package providers

func newGMI(d Deps) Profile {
	return &base{spec: Spec{
		Name: "gmi", Aliases: []string{"gmi-cloud", "gmicloud"}, DisplayName: "GMI Cloud",
		Description: "GMI Cloud — multi-model direct API (slash-form model IDs)",
		SignupURL:   "https://www.gmicloud.ai/", EnvVars: []string{"GMI_API_KEY", "GMI_BASE_URL"},
		BaseURL: "https://api.gmi-serving.com/v1", AuthType: authTypeAPIKey,
		DefaultHeaders:  map[string]string{"User-Agent": userAgent(d, "icebath")},
		DefaultAuxModel: "google/gemini-3.1-flash-lite-preview",
		FallbackModels: []string{"zai-org/GLM-5.1-FP8", "deepseek-ai/DeepSeek-V3.2", "moonshotai/Kimi-K2.5",
			"google/gemini-3.1-flash-lite-preview", "anthropic/claude-sonnet-5", "anthropic/claude-sonnet-4.6", "openai/gpt-5.4"},
	}}
}
