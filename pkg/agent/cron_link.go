package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the agent constructor, hard interrupt, turn explainer, client cleanup and handoff check the cron run path calls
func init() {
	cron.SetAgentFactory(newCronAgent)
	cron.SetRequestHardInterrupt(cronRequestHardInterrupt)
	cron.SetFormatTurnCompletionExplanation(cronTurnCompletionExplanation)
	cron.SetCleanupStaleAsyncClients(provider.CleanupStaleAsyncClients)
	cron.SetMaxIterationHandoffCheck(IsMaxIterationHandoff)
}

// AIAgent in the shape the cron run path drives
type cronAgent struct{ *AIAgent }

// Runs the cron prompt as one conversation under the run task id
func (c cronAgent) RunConversation(ctx context.Context, prompt, taskID string) (map[string]any, error) {
	return c.AIAgent.RunConversation(ctx, prompt, RunConversationOptions{TaskID: taskID})
}

// AIAgent keywords cron passes, the runtime filling the route keywords inside the constructor
func cronAgentOptions(o cron.AgentOptions) AIAgentOptions {
	return AIAgentOptions{
		Model:                    o.Model,
		Runtime:                  o.Runtime,
		MaxIterations:            new(o.MaxIterations),
		ReasoningConfig:          o.ReasoningConfig,
		PrefillMessages:          o.PrefillMessages,
		FallbackModel:            o.FallbackModel,
		CredentialPool:           o.CredentialPool,
		ProvidersAllowed:         o.ProvidersAllowed,
		ProvidersIgnored:         o.ProvidersIgnored,
		ProvidersOrder:           o.ProvidersOrder,
		ProviderSort:             o.ProviderSort,
		OpenRouterMinCodingScore: o.OpenRouterMinCodingScore,
		EnabledToolsets:          o.EnabledToolsets,
		DisabledToolsets:         o.DisabledToolsets,
		QuietMode:                o.QuietMode,
		SkipContextFiles:         o.SkipContextFiles,
		LoadSoulIdentity:         o.LoadSoulIdentity,
		SkipMemory:               o.SkipMemory,
		SkipBackgroundReview:     o.SkipBackgroundReview,
		Platform:                 o.Platform,
		SessionID:                o.SessionID,
		SessionDB:                o.SessionDB,
		Env:                      o.Env,
	}
}

// Agent of one cron run built from the keywords the scheduler resolved
func newCronAgent(ctx context.Context, o cron.AgentOptions) (cron.CronAgent, error) {
	a, err := NewAIAgent(ctx, cronAgentOptions(o))
	if err != nil {
		return nil, err
	}
	return cronAgent{a}, nil
}

// Hard stop of a cron agent with the watchdog reason handed to its tool workers
func cronRequestHardInterrupt(a cron.CronAgent, reason, toolReason string) {
	RequestHardInterruptOf(a, &reason, &toolReason)
}

// Turn completion explanation for a reason, nil cause and model standing for the omitted keywords
func cronTurnCompletionExplanation(ctx context.Context, reason string, cause, model *string) string {
	persistenceCause, modelName := "", ""
	if cause != nil {
		persistenceCause = *cause
	}
	if model != nil {
		modelName = *model
	}
	return formatTurnCompletionExplanation(ctx, reason, persistenceCause, "", modelName)
}
