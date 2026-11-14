package providers

import (
	"context"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type dashScopeProfile struct {
	base
}

func newDashScope(d Deps, s Spec) Profile {
	return &dashScopeProfile{base{spec: s}}
}

func (p *dashScopeProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	return Extras{Body: wire.DashScopePreserveThinking(f.Model)}, nil
}

func alibabaSpec() Spec {
	return Spec{Name: "alibaba", Aliases: []string{"dashscope", "alibaba-cloud", "qwen-dashscope", "aliyun"},
		EnvVars: []string{"DASHSCOPE_API_KEY"}, BaseURL: "https://dashscope-intl.aliyuncs.com/compatible-mode/v1"}
}

func alibabaCNSpec() Spec {
	return Spec{Name: "alibaba-cn", Aliases: []string{"dashscope-cn", "alibaba-cloud-cn"},
		DisplayName: "Alibaba Cloud DashScope (China)", Description: "Alibaba Cloud DashScope, mainland-China endpoint",
		EnvVars: []string{"DASHSCOPE_API_KEY", "DASHSCOPE_CN_BASE_URL"}, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"}
}

func alibabaTokenPlanSpec() Spec {
	return Spec{Name: "alibaba-token-plan", Aliases: []string{"dashscope-token-plan"}, DisplayName: "Alibaba Cloud (Token Plan)",
		Description: "Alibaba Cloud Model Studio Token Plan (flat-token tier)", SignupURL: "https://help.aliyun.com/zh/model-studio/",
		EnvVars: []string{"ALIBABA_TOKEN_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_BASE_URL"},
		BaseURL: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", AuthType: authTypeAPIKey}
}

func alibabaTokenPlanCNSpec() Spec {
	return Spec{Name: "alibaba-token-plan-cn", Aliases: []string{"dashscope-token-plan-cn"},
		DisplayName: "Alibaba Cloud (Token Plan, China)", Description: "Alibaba Cloud Model Studio Token Plan, mainland-China endpoint",
		SignupURL: "https://help.aliyun.com/zh/model-studio/",
		EnvVars:   []string{"ALIBABA_TOKEN_PLAN_CN_API_KEY", "ALIBABA_TOKEN_PLAN_API_KEY", "ALIBABA_TOKEN_PLAN_CN_BASE_URL"},
		BaseURL:   "https://token-plan.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", AuthType: authTypeAPIKey}
}
