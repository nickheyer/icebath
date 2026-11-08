package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sanitized POST request view of the API kwargs without the transport keys
func (a *AIAgent) apiRequestPayloadForHook(apiKwargs map[string]any) any {
	body := make(map[string]any, len(apiKwargs))
	for k, v := range apiKwargs {
		if k != "timeout" && k != "http_client" {
			body[k] = v
		}
	}
	return provider.SanitizeHookPayload(map[string]any{"method": "POST", "body": body})
}

// Token buckets of the raw response usage for post_api_request plugins, nil without a truthy usage
func (a *AIAgent) usageSummaryForAPIRequestHook(response any) map[string]any {
	if response == nil {
		return nil
	}
	rawUsage := state.ToolCallField(response, "usage")
	if !pyrepr.Truthy(rawUsage) {
		return nil
	}
	cu := provider.NormalizeUsage(rawUsage, a.provider, a.apiMode)
	return map[string]any{
		"input_tokens": cu.InputTokens, "output_tokens": cu.OutputTokens, "cache_read_tokens": cu.CacheReadTokens,
		"cache_write_tokens": cu.CacheWriteTokens, "reasoning_tokens": cu.ReasoningTokens, "request_count": cu.RequestCount,
		"prompt_tokens": cu.PromptTokens(), "total_tokens": cu.TotalTokens(),
	}
}

// Sanitized model, upstream provider, finish reason, assistant message and usage view of one response
func (a *AIAgent) apiResponsePayloadForHook(response any, assistantMessage *provider.Response, finishReason string) any {
	var content, finish any
	toolCalls := []any{}
	if assistantMessage != nil && assistantMessage.Message != nil {
		content = assistantMessage.Message.Content
		for _, tc := range assistantMessage.Message.ToolCalls {
			toolCalls = append(toolCalls, tc)
		}
	}
	if finishReason != "" {
		finish = finishReason
	}
	return provider.SanitizeHookPayload(map[string]any{
		"model": state.ToolCallField(response, "model"), "upstream_provider": state.ToolCallField(response, "provider"),
		"finish_reason":     finish,
		"assistant_message": map[string]any{"role": "assistant", "content": content, "tool_calls": toolCalls},
		"usage":             a.usageSummaryForAPIRequestHook(response),
	})
}

// Number a Python arithmetic operand stands for, false for a non numeric value
func numericOperand(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// Keywords the api_request_error hook dispatch requires from its caller
var apiRequestErrorHookRequired = []string{"task_id", "turn_id", "api_request_id", "api_call_count", "api_start_time", "api_kwargs", "error_type", "error_message"}

// Fires api_request_error with the measured duration when a consumer is registered, any failure swallowed
func (a *AIAgent) invokeAPIRequestErrorHook(ctx context.Context, kwargs map[string]any) {
	for _, k := range apiRequestErrorHookRequired {
		if _, ok := kwargs[k]; !ok {
			panic(fmt.Sprintf("TypeError: _invoke_api_request_error_hook() missing 1 required keyword-only argument: '%s'", k))
		}
	}
	defer func() {
		if r := recover(); r != nil && isUserInterruptPanic(r) {
			panic(r)
		}
	}()
	if !lifecycleHasHook(ctx, "api_request_error") {
		return
	}
	endedAt := float64(time.Now().UnixNano()) / 1e9
	startedAt, ok := numericOperand(kwargs["api_start_time"])
	if !ok {
		panic(fmt.Sprintf("TypeError: unsupported operand type(s) for -: 'float' and '%s'", pyrepr.TypeName(kwargs["api_start_time"])))
	}
	apiKwargs, _ := kwargs["api_kwargs"].(map[string]any)
	lifecycleInvokeHook(ctx, "api_request_error", map[string]any{
		"task_id": kwargs["task_id"], "turn_id": kwargs["turn_id"], "api_request_id": kwargs["api_request_id"],
		"session_id": a.sessionID, "platform": a.platform, "model": a.model, "provider": a.provider,
		"base_url": a.baseURL, "api_mode": a.apiMode, "api_call_count": kwargs["api_call_count"],
		"api_duration": endedAt - startedAt, "started_at": kwargs["api_start_time"], "ended_at": endedAt,
		"status_code": kwargs["status_code"], "retry_count": kwargs["retry_count"], "max_retries": kwargs["max_retries"],
		"retryable": kwargs["retryable"], "reason": kwargs["reason"],
		"error":   map[string]any{"type": kwargs["error_type"], "message": kwargs["error_message"]},
		"request": a.apiRequestPayloadForHook(apiKwargs),
	})
}

// Human readable one liner of an API error with the agent provider bound
func (a *AIAgent) summarizeAPIError(err error) string {
	return provider.SummarizeAPIError(err, a.provider)
}

// SummarizeAPIError is the provider bound error summary the stream diagnostics print
func (a *AIAgent) SummarizeAPIError(err error) string { return a.summarizeAPIError(err) }

// Learns the per image token price from the response that priced the messages, false when it teaches nothing
func (a *AIAgent) calibrateFromUsage(ctx context.Context, messages []*state.Message, promptTokens int) (int, bool) {
	_, learned, ok := provider.CalibrateImageTokenCost(ctx, a.model, a.baseURL, a.usageAnchor, messages, promptTokens)
	return learned, ok
}
