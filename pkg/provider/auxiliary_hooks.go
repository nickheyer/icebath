package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	PreAuxiliaryCall  = "pre_auxiliary_call"
	PostAuxiliaryCall = "post_auxiliary_call"
)

// Lifecycle seams for hook presence and dispatch
var (
	auxHasHookFn    = plugins.HasHook
	auxInvokeHookFn = plugins.InvokeHook
	auxHookClock    = time.Now
	// Turn identity seam over the relay runtime current turn
	auxCurrentTurnIdentityFn = relayCurrentTurnIdentity
)

// Session, task, turn and platform of the main turn this call runs under, empty for turn less callers
func auxParentTurnIdentity(ctx context.Context) map[string]any {
	ident := map[string]any{"session_id": "", "task_id": "", "turn_id": "", "platform": ""}
	sessionID, platform, taskID, turnID, ok := auxCurrentTurnIdentityFn(ctx)
	if !ok {
		return ident
	}
	ident["session_id"], ident["platform"], ident["task_id"], ident["turn_id"] = sessionID, platform, taskID, turnID
	return ident
}

// System prompt of an Anthropic, Responses or chat request, empty when none
func auxSystemPrompt(messages []any, kwargs map[string]any) string {
	if s, ok := kwargs["system"].(string); ok {
		return s
	}
	if s, ok := kwargs["instructions"].(string); ok {
		return s
	}
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && first["role"] == "system" {
			if s, ok := first["content"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// Chat completions usage of a response with prompt and total sums, nil without usage
func auxUsageSummary(response any) map[string]any {
	m, ok := response.(map[string]any)
	if !ok || !pyrepr.Truthy(m["usage"]) {
		return nil
	}
	cu := NormalizeUsage(m["usage"], "", "chat_completions")
	return map[string]any{
		"input_tokens": cu.InputTokens, "output_tokens": cu.OutputTokens, "cache_read_tokens": cu.CacheReadTokens,
		"cache_write_tokens": cu.CacheWriteTokens, "reasoning_tokens": cu.ReasoningTokens, "request_count": cu.RequestCount,
		"prompt_tokens": cu.PromptTokens(), "total_tokens": cu.TotalTokens(),
	}
}

// Message and finish reason of the first choice, both nil without choices
func auxFirstChoiceMessage(response any) (any, any) {
	m, ok := response.(map[string]any)
	if !ok {
		return nil, nil
	}
	choices := auxAnyList(m["choices"])
	if len(choices) == 0 {
		return nil, nil
	}
	choice, _ := choices[0].(map[string]any)
	return choice["message"], choice["finish_reason"]
}

// Field of a wire map, nil for anything else
func auxHookField(obj any, name string) any {
	if m, ok := obj.(map[string]any); ok {
		return m[name]
	}
	return nil
}

// List view of a JSON array value of any element type, nil for anything else
func auxAnyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// List view of a Python list value and whether it is one
func auxListValue(v any) ([]any, bool) {
	if v == nil {
		return nil, false
	}
	if reflect.ValueOf(v).Kind() != reflect.Slice {
		return nil, false
	}
	return auxAnyList(v), true
}

// Python len of a list, string or mapping value, zero for None
func auxPyLen(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		return pystr.Len(x)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Map, reflect.Array:
		return rv.Len()
	}
	return 0
}

// Python int of a metadata value, an error where int raises
func auxPyInt(v any) (int, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, fmt.Errorf("cannot convert float %s to integer", pyrepr.Str(x))
		}
		return int(x), nil
	case string:
		n, err := strconv.Atoi(pystr.Strip(x))
		if err != nil {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(x))
		}
		return n, nil
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))
}

// Python exception class name of an auxiliary call error
func auxPyExceptionName(err error) string {
	var timeout *AuxTimeoutError
	var runtime *AuxRuntimeError
	var sdk *AuxSDKError
	switch {
	case errors.As(err, &timeout), errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	case errors.As(err, &runtime):
		return "RuntimeError"
	case errors.As(err, &sdk):
		if sdk.Class != "" {
			return sdk.Class
		}
		return "APIError"
	}
	if name := goErrorTypeName(err); name != "" && name != "errorString" && name != "wrapError" && name != "joinError" {
		return name
	}
	return "Exception"
}

// Arguments of one physical provider attempt that the auxiliary hook pair reports
type auxCallHookArgs struct {
	AuxTask   string
	Metadata  map[string]any
	Client    AuxClient
	Kwargs    map[string]any
	Provider  string
	Model     string
	APIMode   string
	Streaming bool
}

// AuxCallHookArgs exposes the auxiliary hook arguments
type AuxCallHookArgs = auxCallHookArgs

// Reply of a native transport already normalized to message, finish reason and usage
type auxNormalizedReply struct {
	Message      any
	FinishReason any
	Usage        map[string]any
}

// AuxNormalizedReply exposes the normalized auxiliary reply
type AuxNormalizedReply = auxNormalizedReply

// Pre and post auxiliary hook pair of one attempt with the base payload built once
type auxCallHooks struct {
	streaming bool
	kwargs    map[string]any
	startedAt float64
	base      map[string]any
}

// AuxCallHooks exposes the hook pair of one auxiliary attempt
type AuxCallHooks = auxCallHooks

func auxEpochSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// Base payload of the attempt, an error where Python int conversion raises
func newAuxCallHooks(ctx context.Context, a auxCallHookArgs) (*auxCallHooks, error) {
	h := &auxCallHooks{streaming: a.Streaming, kwargs: a.Kwargs, startedAt: auxEpochSeconds(auxHookClock())}
	retry, err := auxPyInt(auxOrNil(a.Metadata["retry_count"]))
	if err != nil {
		return nil, err
	}
	requestID := ""
	if v := a.Metadata["api_request_id"]; pyrepr.Truthy(v) {
		requestID = pyrepr.Str(v)
	}
	baseURL := ""
	if a.Client != nil {
		baseURL = a.Client.BaseURL()
	}
	messages := a.Kwargs["messages"]
	if !pyrepr.Truthy(messages) {
		messages = a.Kwargs["input"]
	}
	h.base = auxParentTurnIdentity(ctx)
	for k, v := range map[string]any{
		"aux_task": a.AuxTask, "api_request_id": requestID, "retry_count": retry, "api_call_count": retry + 1,
		"model": a.Model, "provider": a.Provider, "base_url": baseURL, "api_mode": a.APIMode, "streaming": a.Streaming,
		"started_at": h.startedAt, "message_count": auxPyLen(messages),
	} {
		h.base[k] = v
	}
	if v := a.Metadata["plugin_id"]; pyrepr.Truthy(v) {
		h.base["plugin_id"] = pyrepr.Str(v)
	}
	return h, nil
}

// Python falsy values collapse to nil the way the or 0 idiom reads them
func auxOrNil(v any) any {
	if !pyrepr.Truthy(v) {
		return nil
	}
	return v
}

// Fires pre_auxiliary_call with the request view when anything subscribes
func (h *auxCallHooks) pre(ctx context.Context) {
	if !auxHasHook(ctx, PreAuxiliaryCall) {
		return
	}
	kwargs := h.kwargs
	messages, ok := auxListValue(kwargs["messages"])
	if !ok {
		messages, ok = auxListValue(kwargs["input"])
	}
	if !ok {
		messages = []any{}
	}
	body := map[string]any{}
	for k, v := range kwargs {
		if k != "timeout" && k != "http_client" {
			body[k] = v
		}
	}
	totalChars := 0
	for _, m := range messages {
		content := auxHookField(m, "content")
		if !pyrepr.Truthy(content) {
			continue
		}
		totalChars += pystr.Len(pyrepr.Str(content))
	}
	maxTokens := kwargs["max_tokens"]
	if !pyrepr.Truthy(maxTokens) {
		maxTokens = kwargs["max_completion_tokens"]
	}
	payload := map[string]any{}
	for k, v := range h.base {
		payload[k] = v
	}
	payload["request_messages"] = append([]any(nil), messages...)
	payload["system_prompt"] = auxSystemPrompt(messages, kwargs)
	payload["tool_count"] = auxPyLen(auxOrNil(kwargs["tools"]))
	payload["approx_input_tokens"] = totalChars / 4
	payload["request_char_count"] = totalChars
	payload["max_tokens"] = maxTokens
	payload["request"] = SanitizeHookPayload(map[string]any{"method": "POST", "body": body})
	auxFireHook(ctx, PreAuxiliaryCall, payload)
}

// Fires post_auxiliary_call with the outcome, a streamed or failed attempt carries no reply view
func (h *auxCallHooks) post(ctx context.Context, response any, callErr error, normalized *auxNormalizedReply) {
	if !auxHasHook(ctx, PostAuxiliaryCall) {
		return
	}
	endedAt := auxEpochSeconds(auxHookClock())
	payload := map[string]any{}
	for k, v := range h.base {
		payload[k] = v
	}
	payload["ended_at"] = endedAt
	payload["api_duration"] = math.Max(0, endedAt-h.startedAt)
	payload["error"], payload["error_type"] = nil, nil
	if callErr != nil {
		text := auxPyExceptionName(callErr) + ": " + callErr.Error()
		if runes := []rune(text); len(runes) > 2000 {
			text = string(runes[:2000])
		}
		payload["error"], payload["error_type"] = text, auxPyExceptionName(callErr)
	}
	if callErr != nil || h.streaming {
		for k, v := range map[string]any{"finish_reason": nil, "response_model": nil, "usage": nil, "response": nil,
			"assistant_content_chars": 0, "assistant_tool_call_count": 0} {
			payload[k] = v
		}
		auxFireHook(ctx, PostAuxiliaryCall, payload)
		return
	}
	var message, finishReason any
	var usage map[string]any
	if normalized != nil {
		message, finishReason, usage = normalized.Message, normalized.FinishReason, normalized.Usage
	} else {
		message, finishReason = auxFirstChoiceMessage(response)
		usage = auxUsageSummary(response)
	}
	var content, role any
	var toolCalls any = []any{}
	if message != nil {
		content = auxHookField(message, "content")
		role = auxHookField(message, "role")
		if tc := auxHookField(message, "tool_calls"); pyrepr.Truthy(tc) {
			toolCalls = tc
		}
	}
	if !pyrepr.Truthy(role) {
		role = "assistant"
	}
	var usageValue any
	if usage != nil {
		usageValue = usage
	}
	payload["finish_reason"] = finishReason
	payload["response_model"] = auxHookField(response, "model")
	payload["usage"] = usageValue
	payload["response"] = SanitizeHookPayload(map[string]any{
		"model": auxHookField(response, "model"), "finish_reason": finishReason,
		"assistant_message": map[string]any{"role": role, "content": content, "tool_calls": toolCalls},
		"usage":             usageValue,
	})
	chars := 0
	if s, ok := content.(string); ok {
		chars = pystr.Len(s)
	}
	payload["assistant_content_chars"] = chars
	payload["assistant_tool_call_count"] = auxPyLen(toolCalls)
	auxFireHook(ctx, PostAuxiliaryCall, payload)
}

// Hook presence that treats a failing lifecycle lookup as absent
func auxHasHook(ctx context.Context, name string) (has bool) {
	defer func() {
		if recover() != nil {
			has = false
		}
	}()
	return auxHasHookFn(ctx, name)
}

// Dispatches one event and logs a failing dispatch without propagating it
func auxFireHook(ctx context.Context, name string, payload map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("%s plugin hook failed for aux_task=%s; continuing", name, pyrepr.Str(payload["aux_task"])), "err", fmt.Sprint(r))
		}
	}()
	auxInvokeHookFn(ctx, name, payload)
}

// Fires pre_auxiliary_call for one attempt, nil when nothing subscribes or the payload cannot be built
func startAuxCallHooks(ctx context.Context, a auxCallHookArgs) (hooks *auxCallHooks) {
	if !(auxHasHook(ctx, PreAuxiliaryCall) || auxHasHook(ctx, PostAuxiliaryCall)) {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("pre_auxiliary_call payload build failed; continuing", "err", fmt.Sprint(r))
			hooks = nil
		}
	}()
	h, err := newAuxCallHooks(ctx, a)
	if err != nil {
		slog.Warn("pre_auxiliary_call payload build failed; continuing", "err", err)
		return nil
	}
	h.pre(ctx)
	return h
}

// StartAuxCallHooks fires pre_auxiliary_call for one side call attempt
func StartAuxCallHooks(ctx context.Context, a AuxCallHookArgs) *AuxCallHooks {
	return startAuxCallHooks(ctx, a)
}

// Fires the post_auxiliary_call paired with a started attempt
func finishAuxCallHooks(ctx context.Context, hooks *auxCallHooks, response any, callErr error, normalized *auxNormalizedReply) {
	if hooks == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("post_auxiliary_call payload build failed; continuing", "err", fmt.Sprint(r))
		}
	}()
	hooks.post(ctx, response, callErr, normalized)
}

// FinishAuxCallHooks closes the attempt with post_auxiliary_call
func FinishAuxCallHooks(ctx context.Context, hooks *AuxCallHooks, response any, callErr error, normalized *AuxNormalizedReply) {
	finishAuxCallHooks(ctx, hooks, response, callErr, normalized)
}

// Runs one provider attempt between the auxiliary hook pair, reporting and returning its error
func runWithAuxHooks(ctx context.Context, call func() (any, error), a auxCallHookArgs) (any, error) {
	hooks := startAuxCallHooks(ctx, a)
	var response any
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				finishAuxCallHooks(ctx, hooks, nil, fmt.Errorf("%v", r), nil)
				panic(r)
			}
		}()
		response, err = call()
	}()
	if err != nil {
		finishAuxCallHooks(ctx, hooks, nil, err, nil)
		return nil, err
	}
	finishAuxCallHooks(ctx, hooks, response, nil, nil)
	return response, nil
}
