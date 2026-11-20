package provider

// Profiles the bundled bedrock plugin registers, credentials and listing through the AWS SDK
func bedrockPluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "bedrock", mode: modeBedrockConverse,
		base: "https://bedrock-runtime.us-east-1.amazonaws.com", authType: AuthTypeAWSSDK, noListing: true},
		"aws", "aws-bedrock", "amazon-bedrock", "amazon")}
}

// Profiles the bundled azure-foundry plugin registers, the per resource base URL supplied at setup
func azureFoundryPluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "azure-foundry", display: "Azure Foundry",
		desc: "Microsoft Foundry - OpenAI-compatible endpoint (user-supplied base URL)", signup: "https://ai.azure.com/",
		keys: []string{"AZURE_FOUNDRY_API_KEY", "AZURE_FOUNDRY_BASE_URL"}, authType: AuthTypeAPIKey},
		"azure", "azure-ai-foundry", "azure-ai")}
}

// Profiles the bundled huggingface plugin registers
func huggingfacePluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "huggingface", keys: []string{"HF_TOKEN"}, display: "HuggingFace",
		desc: "HuggingFace Inference API", signup: "https://huggingface.co/settings/tokens",
		fallbacks: []string{"Qwen/Qwen3.5-72B-Instruct", "deepseek-ai/DeepSeek-V3.2"}, base: "https://router.huggingface.co/v1",
		authType: AuthTypeAPIKey}, "hf", "hugging-face", "huggingface-hub")}
}

// Profiles the bundled xiaomi plugin registers, no health probe and no list typed tool content
func xiaomiPluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "xiaomi", keys: []string{"XIAOMI_API_KEY"}, base: "https://api.xiaomimimo.com/v1",
		authType: AuthTypeAPIKey, noHealth: true, vision: true, noVisionTool: true}, "mimo", "xiaomi-mimo")}
}

// Profiles the bundled xai plugin registers
func xaiPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "xai", mode: ModeCodexResponses, keys: []string{"XAI_API_KEY"}, base: "https://api.x.ai/v1",
		authType: AuthTypeAPIKey}, "grok", "x-ai", "x.ai")
	p.LazyHeaders = NewLazyHeaders(HeaderEntry{Key: "User-Agent", Fn: func() string { return UserAgent("Icebath-Agent") }})
	return []Profile{p}
}

// Profiles the bundled kilocode plugin registers
func kilocodePluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "kilocode", keys: []string{"KILOCODE_API_KEY"}, base: "https://api.kilo.ai/api/gateway",
		authType: AuthTypeAPIKey}, "kilo-code", "kilo", "kilo-gateway")
	p.DefaultAuxModel = "google/gemini-3.6-flash"
	return []Profile{p}
}

// Profiles the bundled arcee plugin registers
func arceePluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "arcee", keys: []string{"ARCEEAI_API_KEY"}, base: "https://api.arcee.ai/api/v1",
		authType: AuthTypeAPIKey}, "arcee-ai", "arceeai")}
}

// Profiles the bundled stepfun plugin registers
func stepfunPluginProfiles() []Profile {
	p := bundledRowProfile(profileRow{name: "stepfun", keys: []string{"STEPFUN_API_KEY"}, base: "https://api.stepfun.ai/step_plan/v1",
		authType: AuthTypeAPIKey}, "step", "stepfun-coding-plan")
	p.DefaultAuxModel = "step-3.5-flash"
	return []Profile{p}
}

// Profiles the bundled openai-codex plugin registers, OAuth external with no API key
func openaiCodexPluginProfiles() []Profile {
	return []Profile{bundledRowProfile(profileRow{name: "openai-codex", mode: ModeCodexResponses, base: "https://chatgpt.com/backend-api/codex",
		authType: AuthTypeOAuthExternal}, "codex", "openai_codex")}
}
