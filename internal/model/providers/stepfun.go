package providers

import "github.com/nickheyer/icebath/internal/model/auth"

func newStepFun() Profile {
	return &base{spec: Spec{
		Name: "stepfun", Aliases: []string{"step", "stepfun-coding-plan"}, DefaultAuxModel: "step-3.5-flash",
		EnvVars: []string{"STEPFUN_API_KEY"}, BaseURL: auth.StepFunStepPlanIntlBaseURL,
	}}
}
