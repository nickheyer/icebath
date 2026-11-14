package providers

func newNovita() Profile {
	return &base{spec: Spec{
		Name: "novita", Aliases: []string{"novita-ai", "novitaai"}, DisplayName: "NovitaAI",
		Description: "NovitaAI — AI-native cloud for builders and agents",
		SignupURL:   "https://novita.ai/settings/key-management", EnvVars: []string{"NOVITA_API_KEY", "NOVITA_BASE_URL"},
		BaseURL: "https://api.novita.ai/openai/v1", AuthType: authTypeAPIKey, DefaultAuxModel: "deepseek/deepseek-v3-0324",
		FallbackModels: []string{"moonshotai/kimi-k2.5", "minimax/minimax-m2.7", "zai-org/glm-5", "deepseek/deepseek-v3-0324",
			"deepseek/deepseek-r1-0528", "qwen/qwen3-235b-a22b-fp8"},
	}}
}
