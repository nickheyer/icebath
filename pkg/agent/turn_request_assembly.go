package agent

import (
	"context"
	"fmt"
	"strconv"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runs the MoA reference models and appends their aggregated context to the last user row, failing open
func appendMoaContext(ctx context.Context, a *AIAgent, apiMessages []map[string]any, moaConfig map[string]any, originalUserMessage any) {
	prompt, ok := originalUserMessage.(string)
	if !ok {
		prompt = state.FlattenMessageText(originalUserMessage, "\n")
	}
	referenceModels, _ := moaConfig["reference_models"].([]any)
	aggregator, _ := moaConfig["aggregator"].(map[string]any)
	if aggregator == nil {
		aggregator = map[string]any{}
	}
	var referenceTimeout *float64
	if pyTruthy(moaConfig["reference_timeout"]) {
		f, err := pyFloat(moaConfig["reference_timeout"])
		if err != nil {
			conversationLoopLog().Warn(fmt.Sprintf("MoA context aggregation failed: %v", err))
			return
		}
		referenceTimeout = &f
	}
	policy := "loud"
	if pyTruthy(moaConfig["degraded_reference_policy"]) {
		policy = config.PyStr(moaConfig["degraded_reference_policy"])
	}
	moaContext, err := aggregateMoaContext(ctx, a, prompt, apiMessages, referenceModels, aggregator,
		presetTemperature(moaConfig, "reference_temperature"), presetTemperature(moaConfig, "aggregator_temperature"), referenceTimeout, policy)
	if err != nil {
		conversationLoopLog().Warn(fmt.Sprintf("MoA context aggregation failed: %v", err))
		return
	}
	if moaContext == "" {
		return
	}
	for i := len(apiMessages) - 1; i >= 0; i-- {
		msg := apiMessages[i]
		if msg["role"] != "user" {
			continue
		}
		switch base := msg["content"].(type) {
		case string:
			msg["content"] = base + "\n\n" + moaContext
		case nil:
			if _, present := msg["content"]; !present {
				msg["content"] = "\n\n" + moaContext
			}
		case []any:
			msg["content"] = append(append([]any{}, base...), map[string]any{"type": "text", "text": "\n\n" + moaContext})
		case []map[string]any:
			parts := make([]any, 0, len(base)+1)
			for _, p := range base {
				parts = append(parts, p)
			}
			msg["content"] = append(parts, map[string]any{"type": "text", "text": "\n\n" + moaContext})
		}
		return
	}
}

// Python float() of a config value, numbers and numeric strings
func pyFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(pystr.Strip(n), 64)
		if err != nil {
			return 0, fmt.Errorf("could not convert string to float: '%s'", n)
		}
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%T'", v)
}

// Rebases the pending prepared MoA request onto the new messages or prepares a fresh one
func prepareMoaRequest(a *AIAgent, apiMessages []map[string]any, pending map[string]any) (map[string]any, []map[string]any, map[string]any, error) {
	var prepared map[string]any
	if pending != nil {
		if rebaser, ok := a.client.(moaRebaser); ok {
			prepared = rebaser.RebasePreparedRequest(pending, apiMessages)
		}
		pending = nil
	}
	if prepared == nil {
		if preparer, ok := a.client.(moaPreparer); ok {
			p, err := preparer.PrepareRequest(map[string]any{"messages": apiMessages})
			if err != nil {
				return nil, apiMessages, pending, err
			}
			prepared = p
		}
	}
	if prepared != nil {
		apiMessages = mapRows(prepared["messages"])
	}
	return prepared, apiMessages, pending, nil
}

// Request rows of a messages value as dicts, keeping the row maps
func mapRows(v any) []map[string]any {
	switch rows := v.(type) {
	case []map[string]any:
		return rows
	case []any:
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Builds this iteration's request in the load bearing order, cache plan last, then measures request pressure
func assembleAPIRequest(ctx context.Context, a *AIAgent, s *LoopState) error {
	apiMessages, effectiveSystem := buildAPIMessages(ctx, a, s.Messages, s.CurrentTurnUserIdx, s.ExtPrefetchCache, s.PluginUserContext, s.MoaConfig, s.ActiveSystemPrompt)
	if len(s.MoaConfig) > 0 {
		appendMoaContext(ctx, a, apiMessages, s.MoaConfig, s.OriginalUserMessage)
	}
	if len(a.prefillMessages) > 0 {
		offset := 0
		if len(apiMessages) > 0 && apiMessages[0]["role"] == "system" {
			offset = 1
		}
		for idx, pfm := range a.prefillMessages {
			clone, _ := cloneMessageForSend(pfm).(map[string]any)
			pos := offset + idx
			if pos > len(apiMessages) {
				pos = len(apiMessages)
			}
			apiMessages = append(apiMessages[:pos], append([]map[string]any{clone}, apiMessages[pos:]...)...)
		}
	}
	var incoming map[string]any
	if s.CurrentTurnUserIdx >= 0 && s.CurrentTurnUserIdx < len(s.Messages) {
		incoming = state.MessageToDict(s.Messages[s.CurrentTurnUserIdx])
	}
	conversation := make([]map[string]any, len(s.Messages))
	for i, m := range s.Messages {
		conversation[i] = state.MessageToDict(m)
	}
	apiMessages = applyContextEngineSelection(a, apiMessages, conversation, incoming)
	applyRejectedThinkingSuppression(a, apiMessages)
	apiMessages = sanitizeAPIMessages(ctx, a.sessionID, apiMessages)
	EvictStaleOutboundToolImages(apiMessages, outboundRetiredImageIDs(a))
	if notice := consumePendingSanitizerHealNotice(a.sessionID); notice != "" {
		a.emitWarning(notice)
	}
	crossProtocol := a.apiMode != "codex_responses"
	var nudge *string
	if crossProtocol {
		marker := codexIncompleteNudge
		nudge = &marker
	}
	apiMessages = dropThinkingOnlyAndMergeUsers(apiMessages, crossProtocol, nudge)
	for _, am := range apiMessages {
		if text, ok := am["content"].(string); ok {
			am["content"] = pystr.Strip(text)
		}
	}
	rows := anyRows(apiMessages)
	canonicalizeAPIToolCalls(rows)
	state.SanitizeMessagesSurrogates(rows)
	apiMessages = mapRows(rows)

	var toolsForAPI []map[string]any
	if a.tools != nil {
		toolsForAPI = toolSurface(a.tools)
	}
	if a.usePromptCaching && a.provider != "moa" {
		apiMessages, toolsForAPI = provider.BuildPromptCachePlan(apiMessages, toolsForAPI, provider.PromptCachePlanOptions{
			TTL:                   provider.EffectiveCacheTTL(a.cacheTTL, a.model, a.provider),
			NativeAnthropic:       a.useNativeCacheLayout,
			StaticSystemPrefix:    a.cachedSystemPromptStatic,
			DirectNativeToolCache: a.directNativeAnthropicToolCacheCapability(),
			ToolPartMarkers:       provider.EnvelopeToolPartCacheMarkersSupported(a.provider, a.baseURL),
		})
	}
	s.MoaPreparedRequest = nil
	if a.provider == "moa" {
		prepared, msgs, pending, err := prepareMoaRequest(a, apiMessages, s.PendingMoaPreparedRequest)
		if err != nil {
			return err
		}
		s.MoaPreparedRequest, apiMessages, s.PendingMoaPreparedRequest = prepared, msgs, pending
	}

	asMessages := apiRowsToMessages(anyRows(apiMessages))
	imageCost := provider.CurrentImageTokenCost(ctx)
	var approx int
	switch {
	case !agentStaleThinkingOnWire(a):
		approx = provider.EstimateMessagesTokens(asMessages, imageCost, false)
	case a.apiMode == "anthropic_messages" && provider.NativeAnthropicPreservesPriorThinking(a.baseURL, a.model):
		approx = provider.EstimateNativeAnthropicMessagesTokens(ctx, asMessages)
	default:
		approx = provider.EstimateMessagesTokens(asMessages, imageCost, true)
	}
	s.APIMessages, s.ToolsForAPI = anyRows(apiMessages), toolsForAPI
	pressure := midturnRequestPressureTokens(ctx, a, s.APIMessages, effectiveSystem, approx)
	anchored, ok := provider.AnchoredContextTokens(ctx, s.Messages, a.usageAnchor, true)
	a.requestPressureAnchored = ok
	if ok {
		pressure = anchored
	} else {
		pressure = pressureWithRealFloor(a.contextCompressor, pressure)
	}
	s.ApproxTokens, s.RequestPressureTokens, s.TotalChars = approx, pressure, approx*4
	return nil
}
