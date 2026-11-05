package provider

// Profiles the bundled alibaba plugin registers, the intl and China DashScope routes plus the Token Plan tier
func alibabaPluginProfiles() []Profile {
	out := []Profile{
		bundledRowProfile(profileRow{name: "alibaba", keys: []string{"DASHSCOPE_API_KEY"},
			base: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", authType: AuthTypeAPIKey},
			"dashscope", "alibaba-cloud", "qwen-dashscope", "aliyun"),
		bundledRowProfile(profileRow{name: "alibaba-cn", display: "Alibaba Cloud DashScope (China)",
			desc: "Alibaba Cloud DashScope, mainland-China endpoint", keys: []string{"DASHSCOPE_API_KEY", "DASHSCOPE_CN_BASE_URL"},
			base: "https://dashscope.aliyuncs.com/compatible-mode/v1", authType: AuthTypeAPIKey}, "dashscope-cn", "alibaba-cloud-cn"),
		bundledRowProfile(profileRow{name: "alibaba-token-plan", display: "Alibaba Cloud (Token Plan)",
			desc: "Alibaba Cloud Model Studio Token Plan (flat-token tier)", signup: "https://help.aliyun.com/zh/model-studio/",
			keys: []string{"ALIBABA_TOKEN_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_BASE_URL"},
			base: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", authType: AuthTypeAPIKey}, "dashscope-token-plan"),
		bundledRowProfile(profileRow{name: "alibaba-token-plan-cn", display: "Alibaba Cloud (Token Plan, China)",
			desc: "Alibaba Cloud Model Studio Token Plan, mainland-China endpoint", signup: "https://help.aliyun.com/zh/model-studio/",
			keys: []string{"ALIBABA_TOKEN_PLAN_CN_API_KEY", "ALIBABA_TOKEN_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_CN_BASE_URL"},
			base: "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", authType: AuthTypeAPIKey}, "dashscope-token-plan-cn"),
	}
	for i := range out {
		out[i].BuildAPIKwargsExtrasHook = dashscopeBuildAPIKwargsExtras
	}
	return out
}

// Asks DashScope compatible mode to keep replayed prior turn reasoning
func dashscopeBuildAPIKwargsExtras(_ *Reasoning, kw map[string]any) (map[string]any, map[string]any) {
	return dashscopePreserveThinkingExtras(hookString(kw, "model")), map[string]any{}
}
