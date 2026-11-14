package providers

func alibabaCodingPlanSpec() Spec {
	return Spec{Name: "alibaba-coding-plan", Aliases: []string{"alibaba_coding", "alibaba-coding", "dashscope-coding"},
		DisplayName: "Alibaba Cloud (Coding Plan)", Description: "Alibaba Cloud Coding Plan (Dedicated coding tier)",
		SignupURL: "https://help.aliyun.com/zh/model-studio/",
		EnvVars:   []string{"ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY", "ALIBABA_CODING_PLAN_BASE_URL"},
		BaseURL:   "https://coding-intl.dashscope.aliyuncs.com/v1", AuthType: authTypeAPIKey}
}

func alibabaCodingPlanCNSpec() Spec {
	return Spec{Name: "alibaba-coding-plan-cn", Aliases: []string{"alibaba-coding-cn", "dashscope-coding-cn"},
		DisplayName: "Alibaba Cloud (Coding Plan, China)", Description: "Alibaba Cloud Coding Plan, mainland-China endpoint",
		SignupURL: "https://help.aliyun.com/zh/model-studio/",
		EnvVars:   []string{"ALIBABA_CODING_PLAN_CN_API_KEY", "ALIBABA_CODING_PLAN_API_KEY", "DASHSCOPE_API_KEY", "ALIBABA_CODING_PLAN_CN_BASE_URL"},
		BaseURL:   "https://coding.dashscope.aliyuncs.com/v1", AuthType: authTypeAPIKey}
}
