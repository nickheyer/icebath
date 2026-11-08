package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

var summaryLog = slog.Default().With("logger", "agent.chat_completion_helpers")

const emptySummaryResponse = "I reached the iteration limit and couldn't generate a summary."

// Message keys the summary path drops, the persistence only fields plus the transport foreign carriers
var summaryForeignMessageKeys = func() map[string]bool {
	keys := map[string]bool{"reasoning": true, "finish_reason": true, "tool_name": true, "codex_reasoning_items": true, "codex_message_items": true, "platform_message_id": true}
	for k := range state.PersistenceOnlyMessageFields {
		keys[k] = true
	}
	return keys
}()

// Implemented by the MoA client that remembers the aggregator slot of its last call
type moaAggregatorSlotHolder interface {
	LastAggregatorSlot() map[string]any
}

// IterationSummaryAPIMessages are the wire ready summary messages the agent's request builder produces
func IterationSummaryAPIMessages(agent ChatCompletionAgent, messages []map[string]any) []map[string]any {
	return iterationSummaryAPIMessages(agent, messages)
}

// Wire ready summary messages built like the main loop request, sidecars substituted and scaffolding swept
func iterationSummaryAPIMessages(agent ChatCompletionAgent, messages []map[string]any) []map[string]any {
	needsSanitize := agent.ShouldSanitizeToolCalls()
	sanitizeModel := agent.Model()
	if needsSanitize && agent.Provider() == "moa" {
		if holder, ok := agent.Client().(moaAggregatorSlotHolder); ok {
			if model := holder.LastAggregatorSlot()["model"]; pyrepr.Truthy(model) {
				sanitizeModel = fmt.Sprint(model)
			}
		}
	}
	api := make([]map[string]any, 0, len(messages)+1)
	for _, msg := range messages {
		apiMsg := maps.Clone(msg)
		agent.CopyReasoningContentForAPI(msg, apiMsg)
		for key := range summaryForeignMessageKeys {
			delete(apiMsg, key)
		}
		if apiMsg["role"] == "tool" {
			delete(apiMsg, "name")
		}
		agent.SubstituteAPIContent(apiMsg)
		if needsSanitize {
			agent.SanitizeToolCallsForStrictAPI(apiMsg, sanitizeModel)
		}
		api = append(api, apiMsg)
	}
	system := agent.CachedSystemPrompt()
	if eph := agent.EphemeralSystemPrompt(); eph != "" {
		system = pystr.Strip(system + "\n\n" + eph)
	}
	if system != "" {
		api = append([]map[string]any{{"role": "system", "content": system}}, api...)
	}
	for idx, pfm := range agent.PrefillMessages() {
		pos := idx
		if system != "" {
			pos++
		}
		pos = min(pos, len(api))
		api = append(api[:pos], append([]map[string]any{maps.Clone(pfm)}, api[pos:]...)...)
	}
	api = agent.SanitizeAPIMessages(api)
	agent.EvictStaleOutboundToolImages(api)
	agent.StripImagesForRejectingModel(api)
	api = agent.DropThinkingOnlyAndMergeUsers(api)
	for _, msg := range api {
		for key := range msg {
			if strings.HasPrefix(key, "_") {
				delete(msg, key)
			}
		}
	}
	for _, msg := range api {
		if s, ok := msg["content"].(string); ok {
			msg["content"] = pystr.Strip(s)
		}
	}
	agent.CanonicalizeAPIToolCalls(api)
	cloned := make([]map[string]any, len(api))
	rows := make([]any, len(api))
	for i, msg := range api {
		cloned[i] = agent.CloneMessageForSend(msg)
		rows[i] = cloned[i]
	}
	state.SanitizeMessagesSurrogates(rows)
	return cloned
}

// Runs one summary attempt inside Relay managed execution with the logical completion deferred
func managedSummaryCall(ctx context.Context, agent ChatCompletionAgent, apiRequestID string, request map[string]any, retryCount int) (any, error) {
	name := agent.Provider()
	if name == "" {
		name = "provider"
	}
	mode := agent.APIMode()
	if mode == "" {
		mode = "chat_completions"
	}
	return relayExecuteCurrent(ctx, request, agent.InterruptibleAPICall, name, agent.Model(),
		map[string]any{"api_mode": mode, "api_request_id": apiRequestID, "call_role": "iteration_summary", "retry_count": retryCount}, true)
}

// Reports a chat completion object whose sole message is the router timeout sentinel without generated tokens
func routerTimeoutShimResponse(response any) bool {
	obj, ok := DumpIfModel(response).(map[string]any)
	if !ok {
		return false
	}
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) != 1 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content, ok := message["content"].(string)
	if !ok || pystr.Strip(content) != routerTimeoutShim || pyrepr.Truthy(message["tool_calls"]) {
		return false
	}
	usage, _ := obj["usage"].(map[string]any)
	for _, field := range []string{"completion_tokens", "output_tokens"} {
		switch v := usage[field].(type) {
		case int:
			if v > 0 {
				return false
			}
		case int64:
			if v > 0 {
				return false
			}
		case float64:
			if v > 0 {
				return false
			}
		}
	}
	return true
}

// Normalized summary text, empty for a router timeout shim so the retry slot takes it
func summaryText(agent ChatCompletionAgent, response any, normalizeKwargs map[string]any) (string, error) {
	if routerTimeoutShimResponse(response) {
		summaryLog.Warn("Iteration summary returned a router timeout shim; retrying")
		return "", nil
	}
	normalized, err := agent.Transport().NormalizeResponse(response, normalizeKwargs)
	if err != nil {
		return "", err
	}
	if normalized.Message == nil {
		return "", nil
	}
	if len(normalized.Message.ToolCalls) > 0 {
		summaryLog.Warn("Iteration summary emitted tool calls; discarding them")
	}
	return pystr.Strip(normalized.Message.Text()), nil
}

type summaryAttempt func(ctx context.Context, retryCount int) (string, error)

// Codex summary attempt through the interruptible call seam with the tool keys removed
func codexSummaryAttempt(ctx context.Context, agent ChatCompletionAgent, apiMessages []map[string]any, apiRequestID string) (summaryAttempt, error) {
	return func(ctx context.Context, retryCount int) (string, error) {
		kwargs, err := agent.BuildAPIKwargs(apiMessages)
		if err != nil {
			return "", err
		}
		delete(kwargs, "tools")
		delete(kwargs, "tool_choice")
		delete(kwargs, "parallel_tool_calls")
		response, err := agent.InterruptibleAPICall(ctx, kwargs)
		if err != nil {
			return "", err
		}
		return summaryText(agent, response, nil)
	}, nil
}

// Messages API summary attempt built by the transport with the Nous Portal extra body merged
func anthropicSummaryAttempt(ctx context.Context, agent ChatCompletionAgent, apiMessages []map[string]any, apiRequestID string) (summaryAttempt, error) {
	return func(ctx context.Context, retryCount int) (string, error) {
		params := map[string]any{"max_tokens": nil, "reasoning_config": agent.ReasoningConfig(), "is_oauth": agent.IsAnthropicOAuth(),
			"preserve_dots": agent.AnthropicPreserveDots(), "base_url": agent.AnthropicBaseURL()}
		if mt := agent.MaxTokens(); mt != nil {
			params["max_tokens"] = *mt
		}
		antKw, err := agent.Transport().BuildKwargs(agent.Model(), apiMessages, nil, params)
		if err != nil {
			return "", err
		}
		antKw = MergeNousPortalMessagesExtraBody(ctx, agent, antKw, apiMessages)
		response, err := managedSummaryCall(ctx, agent, apiRequestID, antKw, retryCount)
		if err != nil {
			return "", err
		}
		return summaryText(agent, response, map[string]any{"strip_tool_prefix": agent.IsAnthropicOAuth()})
	}, nil
}

// Chat summary attempt on the main loop kwargs builder, the MoA facade receiving its prepared request
func chatSummaryAttempt(ctx context.Context, agent ChatCompletionAgent, apiMessages []map[string]any, apiRequestID string) (summaryAttempt, error) {
	kwargs, err := agent.BuildAPIKwargs(apiMessages)
	if err != nil {
		return nil, err
	}
	agent.SanitizeOutboundKwargs(kwargs)
	if facade, ok := agent.Client().(moaPrepareFacade); ok && agent.Provider() == "moa" {
		prepared, err := facade.PrepareRequest(apiMessages)
		if err != nil {
			return nil, err
		}
		kwargs["_moa_prepared_request"] = prepared
	}
	return func(ctx context.Context, retryCount int) (string, error) {
		response, err := managedSummaryCall(ctx, agent, apiRequestID, kwargs, retryCount)
		if err != nil {
			return "", err
		}
		return summaryText(agent, response, nil)
	}, nil
}

var summaryAttemptBuilders = map[string]func(context.Context, ChatCompletionAgent, []map[string]any, string) (summaryAttempt, error){
	"codex_responses": codexSummaryAttempt, "anthropic_messages": anthropicSummaryAttempt,
}

var summaryThinkBlockRe = regexp.MustCompile(`(?s)<think>.*?</think>[\s\pZ\x{1c}-\x{1f}\x{85}]*`)

var newSummaryRequestID = func() string { return "iteration-summary:" + uuid.NewString() }

// Summary of the turn once max iterations is reached, the final response text of the turn
func HandleMaxIterations(ctx context.Context, agent ChatCompletionAgent, messages *[]map[string]any) (final string, err error) {
	warning := fmt.Sprintf("⚠️  Reached maximum iterations (%d). Requesting summary...", agent.MaxIterations())
	if agent.SuppressStatusOutput() {
		summaryLog.Warn(warning)
	} else {
		agent.SafePrint(warning, true)
	}
	requestID := newSummaryRequestID()
	outcome := "failed"
	nudge := state.AppendMessage(messages, map[string]any{"role": "user", "content": state.MaxIterationsSummaryRequest}, nil)
	defer func() {
		if cerr := relayCompleteLogicalCall(ctx, requestID, outcome, "", "", "", ""); cerr != nil && err == nil {
			final, err = "", cerr
		}
	}()
	final, err = summarizeAtIterationLimit(ctx, agent, messages, requestID, &outcome)
	var interrupted *InterruptedError
	switch {
	case err == nil:
		return final, nil
	case errors.As(err, &interrupted):
		outcome = "cancelled"
		if n := len(*messages); n > 0 && reflect.ValueOf((*messages)[n-1]).Pointer() == reflect.ValueOf(nudge).Pointer() {
			*messages = (*messages)[:n-1]
		}
		return "", err
	}
	summaryLog.Warn(fmt.Sprintf("Failed to get summary response: %v", err))
	return tools.SiteCopy(ctx, "max_iterations_no_summary", map[string]any{"limit": agent.MaxIterations()})
}

// One retry on an empty summary, a summary empty once its think block is stripped is not retried
func summarizeAtIterationLimit(ctx context.Context, agent ChatCompletionAgent, messages *[]map[string]any, requestID string, outcome *string) (string, error) {
	apiMessages := iterationSummaryAPIMessages(agent, *messages)
	build, ok := summaryAttemptBuilders[agent.APIMode()]
	if !ok {
		build = chatSummaryAttempt
	}
	attempt, err := build(ctx, agent, apiMessages, requestID)
	if err != nil {
		return "", err
	}
	for retryCount := range 2 {
		text, err := attempt(ctx, retryCount)
		if err != nil {
			return "", err
		}
		if text == "" {
			continue
		}
		if strings.Contains(text, "<think>") {
			text = pystr.Strip(summaryThinkBlockRe.ReplaceAllString(text, ""))
		}
		if text != "" {
			*outcome = "success"
			state.AppendMessage(messages, map[string]any{"role": "assistant", "content": text}, nil)
			return text, nil
		}
		break
	}
	return emptySummaryResponse, nil
}
