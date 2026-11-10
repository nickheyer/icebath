package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Sets one extra header on a copy of the extra headers map, which the transport defaults may share
func setExtraHeader(apiKwargs map[string]any, key string, value any) {
	headers := map[string]any{}
	switch existing := apiKwargs["extra_headers"].(type) {
	case map[string]any:
		for k, v := range existing {
			headers[k] = v
		}
	case map[string]string:
		for k, v := range existing {
			headers[k] = v
		}
	}
	headers[key] = value
	apiKwargs["extra_headers"] = headers
}

// Consumes the user initiated turn flag once and applies the route's user initiated request headers
func applyUserTurnRequestHeaders(ctx context.Context, a *AIAgent, apiKwargs map[string]any) {
	if !a.isUserInitiatedTurn {
		return
	}
	a.isUserInitiatedTurn = false
	answer := provider.RouteAnswer(ctx, "request_headers", a.provider, a, map[string]any{
		"base_url": a.baseURL, "model": a.model, "user_initiated": true})
	switch headers := answer.(type) {
	case map[string]string:
		for k, v := range headers {
			setExtraHeader(apiKwargs, k, v)
		}
	case map[string]any:
		for k, v := range headers {
			setExtraHeader(apiKwargs, k, v)
		}
	}
}

// Fires the pre_api_request lifecycle hook with shallow copies of the request and transcript, swallowing hook failures
func firePreAPIRequestHook(ctx context.Context, a *AIAgent, s *LoopState, apiKwargs map[string]any, apiMessages []map[string]any, trace []any) {
	defer func() {
		if r := recover(); r != nil && isUserInterruptPanic(r) {
			panic(r)
		}
	}()
	if !lifecycleHasHook(ctx, "pre_api_request") {
		return
	}
	var requestMessages any = apiKwargs["messages"]
	if _, ok := requestMessages.([]any); !ok {
		requestMessages = apiKwargs["input"]
	}
	if _, ok := requestMessages.([]any); !ok {
		requestMessages = anyRows(apiMessages)
	}
	requestList := append([]any{}, requestMessages.([]any)...)
	history := make([]any, len(s.Messages))
	for i, m := range s.Messages {
		history[i] = state.MessageToDict(m)
	}
	lifecycleInvokeHook(ctx, "pre_api_request", map[string]any{
		"task_id": s.EffectiveTaskID, "turn_id": s.TurnID, "api_request_id": s.APIRequestID, "session_id": a.sessionID,
		"user_message": s.OriginalUserMessage, "conversation_history": history, "platform": a.platform, "model": a.model,
		"provider": a.provider, "base_url": a.baseURL, "api_mode": a.apiMode, "api_call_count": s.APICallCount,
		"retry_count": s.RetryCount, "request_messages": requestList, "system_prompt": systemPromptForHooks(apiKwargs, requestMessages),
		"message_count": len(apiMessages), "tool_count": len(a.tools), "approx_input_tokens": s.ApproxTokens,
		"request_char_count": s.TotalChars, "max_tokens": a.maxTokens, "started_at": s.APIStartTime,
		"middleware_trace": append([]any{}, trace...), "request": a.apiRequestPayloadForHook(apiKwargs),
	})
}

// Runs the LLM request middleware chain, sending the request unmodified when a middleware fails
func applyLLMRequestMiddleware(ctx context.Context, a *AIAgent, s *LoopState, apiKwargs map[string]any) (payload, original map[string]any, trace []any) {
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			conversationLoopLog().Warn("LLM request middleware failed; sending the request unmodified", "error", fmt.Sprint(r))
			original = make(map[string]any, len(apiKwargs))
			for k, v := range apiKwargs {
				original[k] = v
			}
			payload, trace = apiKwargs, []any{}
		}
	}()
	res := plugins.GetPluginManager(ctx).ApplyLLMRequestMiddleware(apiKwargs, map[string]any{
		"task_id": s.EffectiveTaskID, "turn_id": s.TurnID, "api_request_id": s.APIRequestID, "session_id": a.sessionID,
		"platform": a.platform, "model": a.model, "provider": a.provider, "base_url": a.baseURL, "api_mode": a.apiMode,
		"api_call_count": s.APICallCount,
	})
	payload, _ = res.Payload.(map[string]any)
	original, _ = res.OriginalPayload.(map[string]any)
	trace = make([]any, len(res.Trace))
	for i, t := range res.Trace {
		trace[i] = t
	}
	return payload, original, trace
}

// Builds the provider payload for the current route through the request middleware, firing no turn hook
func buildRequestPayload(ctx context.Context, a *AIAgent, s *LoopState) error {
	a.resetStreamDeliveryTracking()
	a.lastAPIFirstChunkAt = nil
	apiMessages := mapRows(s.APIMessages)
	a.reapplyReasoningEchoForProvider(apiMessages)
	apiMessages, s.MoaPreparedRequest, s.ToolsForAPI = redecoratePromptCacheForProvider(ctx, a, apiMessages, s.SystemMessage, s.MoaPreparedRequest, s.ToolsForAPI)
	rows := anyRows(apiMessages)
	a.stripImagesForRejectingModel(&rows)
	apiMessages = mapRows(rows)
	s.APIMessages = rows
	observability.ObserveRequestTools(ctx, a, s.ToolsForAPI)
	apiKwargs, err := a.buildAPIKwargs(apiMessages, s.ToolsForAPI, !sameToolSurface(s.ToolsForAPI, a.tools))
	if err != nil {
		return err
	}
	a.sanitizeOutboundKwargs(apiKwargs)
	if a.apiMode == "codex_responses" {
		if apiKwargs, err = provider.PreflightKwargs(a.getTransport(""), apiKwargs, false, provider.DropsReplayItemIDs(a.baseURL), a.isCodexBackend()); err != nil {
			return err
		}
	}
	if a.emptyContentRetries > 0 && a.isOpenRouterURL() {
		setExtraHeader(apiKwargs, "X-OpenRouter-Cache", "false")
	}
	applyUserTurnRequestHeaders(ctx, a, apiKwargs)
	s.APIKwargs, s.OriginalAPIKwargs, s.LLMMiddlewareTrace = applyLLMRequestMiddleware(ctx, a, s, apiKwargs)
	return nil
}

// True when the request tools are the agent's registry list itself, element for element
func sameToolSurface(toolsForAPI []map[string]any, agentTools []any) bool {
	if len(toolsForAPI) != len(agentTools) || (toolsForAPI == nil) != (agentTools == nil) {
		return false
	}
	for i, t := range agentTools {
		m, ok := t.(map[string]any)
		if !ok || fmt.Sprintf("%p", m) != fmt.Sprintf("%p", toolsForAPI[i]) {
			return false
		}
	}
	return true
}

// Builds the attempt's request in order, every mutation before middleware, hooks and debug dumps observe it
func buildAPIRequest(ctx context.Context, a *AIAgent, s *LoopState) error {
	if err := buildRequestPayload(ctx, a, s); err != nil {
		return err
	}
	firePreAPIRequestHook(ctx, a, s, s.APIKwargs, mapRows(s.APIMessages), s.LLMMiddlewareTrace)
	if config.EnvVarEnabled("ICEBATH_DUMP_REQUESTS") {
		a.dumpAPIRequestDebug(tools.WithEnvHome(ctx, a.env), s.APIKwargs, "preflight", nil)
	}
	if s.MoaPreparedRequest != nil && a.provider == "moa" {
		if moaClientConsumesPreparedRequest(a.client) {
			s.APIKwargs["_moa_prepared_request"] = s.MoaPreparedRequest
		} else {
			conversationLoopLog().Warn(fmt.Sprintf("MoA client replaced mid-turn (client=%s); sending the prepared prompt without the MoA handshake", pyrepr.TypeName(a.client)))
		}
	}
	return nil
}
