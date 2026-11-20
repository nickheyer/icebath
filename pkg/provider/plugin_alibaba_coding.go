package provider

// Profiles the bundled alibaba-coding-plan plugin registers, the CN key checked first with the shared vars as fallbacks
func alibabaCodingPluginProfiles() []Profile {
	out := []Profile{
		bundledRowProfile(profileRow{name: "alibaba-coding-plan", display: "Alibaba Cloud (Coding Plan)",
			desc: "Alibaba Cloud Coding Plan (Dedicated coding tier)", signup: "https://help.aliyun.com/zh/model-studio/",
			keys: []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY", "ALIBABA_CODING_PLAN_BASE_URL"},
			base: "https://coding-intl.dashscope.aliyuncs.com/v1", authType: AuthTypeAPIKey}, "alibaba_coding", "alibaba-coding", "dashscope-coding"),
		bundledRowProfile(profileRow{name: "alibaba-coding-plan-cn", display: "Alibaba Cloud (Coding Plan, China)",
			desc: "Alibaba Cloud Coding Plan, mainland-China endpoint", signup: "https://help.aliyun.com/zh/model-studio/",
			keys: []string{"ALIBABA_CODING_PLAN_CN_API_KEY", "ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY", "ALIBABA_CODING_PLAN_CN_BASE_URL"},
			base: "https://coding.dashscope.aliyuncs.com/v1", authType: AuthTypeAPIKey}, "alibaba-coding-cn", "dashscope-coding-cn"),
	}
	for i := range out {
		out[i].BuildAPIKwargsExtrasHook = dashscopeBuildAPIKwargsExtras
	}
	return out
}
