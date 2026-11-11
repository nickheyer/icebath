package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// SwitchModel swaps the live route in place, a once pick keeping its alias for the turn and a restore clearing the one turn state
func (a *AIAgent) SwitchModel(ctx context.Context, newModel, newProvider, apiKey, baseURL, apiMode string, capabilities map[string]bool, once, restore bool, alias string) error {
	if err := a.switchModel(ctx, newModel, newProvider, apiKey, baseURL, apiMode, capabilities); err != nil {
		return err
	}
	switch {
	case restore:
		a.SetOneTurn(false, nil)
	case once:
		a.oneTurn = true
		a.oneTurnAlias = alias
	}
	return nil
}

// RestoreReasoning puts a one turn level back on this agent only and drops the turn's own level
func (a *AIAgent) RestoreReasoning(cfg map[string]any) {
	a.reasoningConfig = cfg
	a.oneTurnSessionReasoning, a.oneTurnSessionReasoningSet = nil, false
}

// CustomProviderEntries is the custom provider list the agent was built with
func (a *AIAgent) CustomProviderEntries() []any { return a.customProviders }

// SetReasoningConfig replaces the reasoning config the next request sends
func (a *AIAgent) SetReasoningConfig(cfg map[string]any) { a.reasoningConfig = cfg }

// RestorePrimarySnapshot reinstates a captured primary runtime as an active fallback and restores it, true when the route came back
func (a *AIAgent) RestorePrimarySnapshot(ctx context.Context, primary map[string]any) (bool, error) {
	copied, _ := pycopy.DeepCopy(primary).(map[string]any)
	a.primaryRuntime = copied
	a.fallbackActivated = true
	a.rateLimitedUntil = 0
	return a.restorePrimaryRuntime(ctx)
}

// Trigger the compressor will use after a switch, the plain ratio over the floor for engines without a preview
func switchThresholdTokens(engine ContextEngine, model string, contextLength int, providerName string) int {
	if cc, ok := engine.(*ContextCompressor); ok {
		return cc.PreviewThresholdTokens(model, contextLength, providerName)
	}
	percent := engine.Base().ThresholdPercent
	return max(int(float64(contextLength)*percent), provider.MinimumContextLength)
}

// Rough request size of messages past the protected window, else the last billed prompt, false without either
func (a *AIAgent) switchEstimateTokens(ctx context.Context, messages []map[string]any) (int, bool) {
	cc := a.contextCompressor
	if cc == nil {
		return 0, false
	}
	base := cc.Base()
	if messages != nil {
		if len(messages) <= base.ProtectFirstN+base.ProtectLastN+1 {
			return 0, false
		}
		var schemas []map[string]any
		for _, t := range a.tools {
			if m, ok := t.(map[string]any); ok {
				schemas = append(schemas, m)
			}
		}
		return estimateRequestRowsRough(ctx, messages, a.cachedSystemPrompt, schemas), true
	}
	if base.LastPromptTokens > 0 {
		return base.LastPromptTokens, true
	}
	return 0, false
}

// Ineffective compression count of the built in compressor, zero for other engines
func ineffectiveCompressions(engine ContextEngine) int {
	if cc, ok := engine.(*ContextCompressor); ok {
		return cc.ineffectiveCompressionCount
	}
	return 0
}

// Appends a warning to a switch result, joined with a bar when one is already there
func appendSwitchWarning(result *provider.ModelSwitchResult, text string) {
	if result.WarningMessage != "" {
		result.WarningMessage = result.WarningMessage + " | " + text
		return
	}
	result.WarningMessage = text
}

// Python thousands grouping of an int
func switchGroupedInt(n int) string { return pystr.GroupThousands(int64(n)) }

// Configured route a gateway switch resolves the display context against, nil fields falling back to the agent
type PreflightConfigured struct {
	Model, Provider, BaseURL *string
}

// MergePreflightCompressionWarning warns on result when the next message after the switch will preflight compress
func (a *AIAgent) MergePreflightCompressionWarning(ctx context.Context, result *provider.ModelSwitchResult, messages []map[string]any, customProviders []any, configContextLength *int, configured PreflightConfigured) {
	if !result.Success || a == nil || !a.compressionEnabled || a.contextCompressor == nil {
		return
	}
	cc := a.contextCompressor
	if customProviders == nil {
		customProviders = a.customProviders
	}
	var providers []map[string]any
	for _, p := range customProviders {
		if m, ok := p.(map[string]any); ok {
			providers = append(providers, m)
		}
	}
	oldCtx := cc.Base().ContextLength
	baseURL, apiKey := result.BaseURL, result.APIKey
	if baseURL == "" {
		baseURL = a.baseURL
	}
	if apiKey == "" {
		apiKey = a.apiKey
	}
	newCtx, ok := provider.ResolveDisplayContextLength(ctx, provider.DisplayContextRequest{
		Model: result.NewModel, Provider: result.TargetProvider, BaseURL: baseURL, APIKey: apiKey,
		ModelInfo: result.ModelInfo, CustomProviders: providers, ConfigContextLength: configContextLength,
		ConfiguredModel: orAgentValue(configured.Model, a.model), ConfiguredProvider: orAgentValue(configured.Provider, a.provider),
		ConfiguredBaseURL: orAgentValue(configured.BaseURL, a.baseURL)})
	if !ok || newCtx == 0 {
		return
	}
	estimate, ok := a.switchEstimateTokens(ctx, messages)
	if !ok {
		return
	}
	newThreshold := switchThresholdTokens(cc, result.NewModel, newCtx, result.TargetProvider)
	if estimate < newThreshold || ineffectiveCompressions(cc) >= 2 {
		return
	}
	text := ""
	if oldCtx != 0 && newCtx < oldCtx {
		text = fmt.Sprintf("Context window shrinks (%s → %s). ", switchGroupedInt(oldCtx), switchGroupedInt(newCtx))
	}
	text += fmt.Sprintf("Session is ~%s tokens; %s allows %s (auto-compress at ~%s). Your next message will run preflight compression before the model replies.",
		switchGroupedInt(estimate), result.NewModel, switchGroupedInt(newCtx), switchGroupedInt(newThreshold))
	appendSwitchWarning(result, text)
}

// SyncToSession points the built agent at another session after a resume or branch switch, keeping memory lineage
func (a *AIAgent) SyncToSession(ctx context.Context, sessionID, parentSessionID, reason string, historyLen int) {
	a.sessionID = sessionID
	a.resetSessionState(nil, "", false)
	a.lastFlushedDBIdx = historyLen
	a.todoStore = tools.NewTodoStore()
	_ = InvalidateSystemPrompt(ctx, a)
	func() {
		defer func() { _ = recover() }()
		if a.memoryManager != nil {
			a.memoryManager.OnSessionSwitch(ctx, sessionID, memory.SessionSwitchOptions{ParentSessionID: parentSessionID, Reason: reason})
		}
	}()
}

// Configured value when given, else the agent's own
func orAgentValue(value *string, agentValue string) string {
	if value != nil {
		return *value
	}
	return agentValue
}
