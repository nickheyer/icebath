package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Responses input the current route replays under native compaction, false when ineligible or conversion fails
func nativeResponsesReplayItems(a *AIAgent, messages []*state.Message) ([]map[string]any, bool) {
	if a.apiMode != "codex_responses" || messages == nil {
		return nil, false
	}
	route := provider.ClassifyResponsesRoute(a.provider, a.baseURL)
	if len(a.nativeCompactionContextManagement(route)) == 0 {
		return nil, false
	}
	model := a.model
	if v, ok := a.EffectiveRequestOverrides()["model"]; ok {
		model, _ = v.(string)
	}
	items, err := provider.NativeResponsesReplayInput(messages, route, a.baseURL, model, a.codexReasoningReplayEnabled)
	if err != nil {
		agentRuntimeHelpersLog().Debug("native Responses replay conversion failed; using the generic fallback", "error", err)
		return nil, false
	}
	return items, true
}

// Whether the current route would replay a persisted native compaction checkpoint from the history
func hasReplayableNativeCompactionCheckpoint(a *AIAgent, history []map[string]any) bool {
	if history == nil {
		return false
	}
	messages := make([]*state.Message, len(history))
	for i, m := range history {
		messages[i] = state.MessageFromDict(m)
	}
	items, ok := nativeResponsesReplayItems(a, messages)
	return ok && provider.HasCompactionCheckpoint(items)
}

// Token estimate of the checkpoint pruned Responses payload, false when native compaction is not proven eligible
func estimateNativeResponsesPreflightTokens(ctx context.Context, a *AIAgent, messages []*state.Message, systemPrompt string, toolDefs []map[string]any) (int, bool) {
	items, ok := nativeResponsesReplayItems(a, messages)
	if !ok {
		return 0, false
	}
	total := 0
	if systemPrompt != "" {
		total = provider.EstimateTokens(systemPrompt)
	}
	if len(items) > 0 {
		total += provider.EstimateRowsTokens(items, provider.CurrentImageTokenCost(ctx))
	}
	if len(toolDefs) > 0 {
		total += provider.EstimateToolsTokens(toolDefs)
	}
	return total, true
}
