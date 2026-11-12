package app

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Selection facts of the gateway agent from its last billed prompt, nil without a positive measurement
func selectionContextForAgent(a *GatewayAgent) *provider.SelectionContext {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	tokens, model := a.compressor.lastPromptTokens, a.model
	a.mu.Unlock()
	if tokens <= 0 {
		return nil
	}
	return &provider.SelectionContext{ContextTokens: tokens, CurrentModel: model}
}

// Warns on a switch result when the next message after the switch will preflight compress on the gateway agent
func mergeGatewayPreflightWarning(ctx context.Context, a *GatewayAgent, result *provider.ModelSwitchResult, messages []map[string]any,
	customProviders []map[string]any, configContextLength *int) {
	if !result.Success || a == nil {
		return
	}
	a.mu.Lock()
	oldCtx, lastPrompt, model, providerName, baseURL := a.compressor.contextLength, a.compressor.lastPromptTokens, a.model, a.provider, a.baseURL
	a.mu.Unlock()
	if result.BaseURL != "" {
		baseURL = result.BaseURL
	}
	newCtx, ok := provider.ResolveDisplayContextLength(ctx, provider.DisplayContextRequest{Model: result.NewModel, Provider: result.TargetProvider,
		BaseURL: baseURL, APIKey: result.APIKey, ModelInfo: result.ModelInfo, CustomProviders: customProviders, ConfigContextLength: configContextLength,
		ConfiguredModel: model, ConfiguredProvider: providerName, ConfiguredBaseURL: a.BaseURL()})
	if !ok || newCtx == 0 {
		return
	}
	estimate := 0
	if messages != nil {
		if len(messages) <= 3+20+1 {
			return
		}
		estimate = provider.EstimateRequestTokens(ctx, agent.RowsToMessages(messages), "", a.Tools(), true)
	} else if lastPrompt > 0 {
		estimate = lastPrompt
	} else {
		return
	}
	newThreshold := max(int(float64(newCtx)*0.5), provider.MinimumContextLength)
	if estimate < newThreshold {
		return
	}
	text := ""
	if oldCtx != 0 && newCtx < oldCtx {
		text = fmt.Sprintf("Context window shrinks (%s → %s). ", pystr.GroupThousands(int64(oldCtx)), pystr.GroupThousands(int64(newCtx)))
	}
	text += fmt.Sprintf("Session is ~%s tokens; %s allows %s (auto-compress at ~%s). Your next message will run preflight compression before the model replies.",
		pystr.GroupThousands(int64(estimate)), result.NewModel, pystr.GroupThousands(int64(newCtx)), pystr.GroupThousands(int64(newThreshold)))
	if result.WarningMessage != "" {
		result.WarningMessage += " | " + text
	} else {
		result.WarningMessage = text
	}
}
