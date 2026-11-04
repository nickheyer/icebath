package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// Lifecycle hook names fired around every inference request
const (
	HookPreAPIRequest   = "pre_api_request"
	HookPostAPIRequest  = "post_api_request"
	HookAPIRequestError = "api_request_error"
)

// Env var overriding the serialized hook payload cap, floor 1000 chars
const HookPayloadMaxCharsEnv = "ICEBATH_PLUGIN_PAYLOAD_MAX_CHARS"

const defaultHookPayloadMaxChars = 50000

var sensitiveHookKeys = map[string]bool{"api_key": true, "authorization": true, "proxy_authorization": true, "cookie": true, "set_cookie": true}

// Hooks receives the request, response, and error payloads of every API call
type Hooks interface {
	Has(name string) bool
	PreRequest(ctx context.Context, p *PreRequestPayload)
	PostRequest(ctx context.Context, p *PostRequestPayload)
	Error(ctx context.Context, p *ErrorPayload)
}

// HookIdentity names the task, turn, request, session, and route a hook payload belongs to
type HookIdentity struct {
	TaskID       string
	TurnID       string
	APIRequestID string
	SessionID    string
	Platform     string
	Model        string
	Provider     string
	BaseURL      string
	APIMode      APIMode
	APICallCount int
}

// PreRequestPayload is the pre_api_request hook argument set
type PreRequestPayload struct {
	HookIdentity
	UserMessage         any
	ConversationHistory []*state.Message
	RetryCount          int
	RequestMessages     []*state.Message
	SystemPrompt        string
	MessageCount        int
	ToolCount           int
	ApproxInputTokens   int
	RequestCharCount    int
	MaxTokens           int
	StartedAt           time.Time
	MiddlewareTrace     []string
	Request             any
}

// PostRequestPayload is the post_api_request hook argument set
type PostRequestPayload struct {
	HookIdentity
	APIDuration            time.Duration
	StartedAt              time.Time
	EndedAt                time.Time
	FirstChunkAt           time.Time
	FinishReason           string
	MessageCount           int
	ResponseModel          string
	Response               any
	Usage                  map[string]any
	ContextLength          int
	AssistantMessage       *state.Message
	AssistantContentChars  int
	AssistantToolCallCount int
	MoAReferences          any
}

// ErrorPayload is the api_request_error hook argument set
type ErrorPayload struct {
	HookIdentity
	APIDuration  time.Duration
	StartedAt    time.Time
	EndedAt      time.Time
	StatusCode   int
	RetryCount   int
	MaxRetries   int
	Retryable    bool
	Reason       FailoverReason
	ErrorType    string
	ErrorMessage string
	Request      any
}

// HookPayloadMaxChars reads the payload cap from the environment with a 1000 char floor
func HookPayloadMaxChars() int {
	raw := os.Getenv(HookPayloadMaxCharsEnv)
	if raw == "" {
		return defaultHookPayloadMaxChars
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return defaultHookPayloadMaxChars
	}
	if n < 1000 {
		return 1000
	}
	return n
}

// IsSensitiveHookKey reports a credential bearing key such as api_key, authorization, or any _api_key suffix
func IsSensitiveHookKey(key string) bool {
	lowered := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	return sensitiveHookKeys[lowered] || strings.HasSuffix(lowered, "_api_key")
}

// HookJSONCaps bounds the depth, string length, and sequence length of a sanitized payload
type HookJSONCaps struct {
	MaxDepth    int
	MaxString   int
	MaxSequence int
}

// DefaultHookJSONCaps matches depth 8, 8000 char strings, and 200 item sequences
var DefaultHookJSONCaps = HookJSONCaps{MaxDepth: 8, MaxString: 8000, MaxSequence: 200}

func (c HookJSONCaps) filled() HookJSONCaps {
	if c.MaxDepth <= 0 {
		c.MaxDepth = DefaultHookJSONCaps.MaxDepth
	}
	if c.MaxString <= 0 {
		c.MaxString = DefaultHookJSONCaps.MaxString
	}
	if c.MaxSequence <= 0 {
		c.MaxSequence = DefaultHookJSONCaps.MaxSequence
	}
	return c
}

func truncateRunes(s string, n int) (string, int) {
	r := []rune(s)
	if len(r) <= n {
		return s, 0
	}
	return string(r[:n]), len(r) - n
}

func typeName(v any) string {
	if v == nil {
		return "NoneType"
	}
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Name() != "" {
		return t.Name()
	}
	return t.String()
}

// HookJSONable coerces any value into JSON safe data with redacted secret keys and bounded size
func HookJSONable(value any, caps HookJSONCaps) any {
	return hookJSONable(value, 0, caps.filled())
}

func hookJSONable(value any, depth int, caps HookJSONCaps) any {
	if depth > caps.MaxDepth {
		return fmt.Sprintf("<%s depth limit>", typeName(value))
	}
	recurse := func(item any) any { return hookJSONable(item, depth+1, caps) }
	switch v := value.(type) {
	case nil:
		return nil
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case json.Number:
		return v
	case string:
		cut, dropped := truncateRunes(v, caps.MaxString)
		if dropped > 0 {
			return cut + fmt.Sprintf("...[truncated %d chars]", dropped)
		}
		return v
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(v))
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return fmt.Sprintf("<%d bytes>", len(v))
		}
		return recurse(decoded)
	case time.Time:
		return v.Format(time.RFC3339Nano)
	case time.Duration:
		return v.Seconds()
	case error:
		s, _ := truncateRunes(v.Error(), caps.MaxString)
		return s
	case map[string]any:
		return hookJSONableMap(reflect.ValueOf(v), recurse, caps)
	case map[string]string:
		return hookJSONableMap(reflect.ValueOf(v), recurse, caps)
	case []any:
		return hookJSONableSeq(reflect.ValueOf(v), recurse, caps)
	case []string:
		return hookJSONableSeq(reflect.ValueOf(v), recurse, caps)
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		return hookJSONableMap(rv, recurse, caps)
	case reflect.Slice, reflect.Array:
		return hookJSONableSeq(rv, recurse, caps)
	case reflect.Struct:
		raw, err := json.Marshal(rv.Interface())
		if err == nil {
			var decoded any
			if json.Unmarshal(raw, &decoded) == nil {
				return recurse(decoded)
			}
		}
		s, _ := truncateRunes(fmt.Sprint(value), caps.MaxString)
		return s
	case reflect.String:
		return recurse(rv.String())
	case reflect.Bool:
		return rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return rv.Uint()
	case reflect.Float32, reflect.Float64:
		return rv.Float()
	}
	s, _ := truncateRunes(fmt.Sprint(value), caps.MaxString)
	return s
}

func hookJSONableMap(rv reflect.Value, recurse func(any) any, caps HookJSONCaps) any {
	out := map[string]any{}
	keys := rv.MapKeys()
	for idx, k := range keys {
		if idx >= caps.MaxSequence {
			out["_truncated_items"] = len(keys) - caps.MaxSequence
			break
		}
		strKey := fmt.Sprint(k.Interface())
		if IsSensitiveHookKey(strKey) {
			out[strKey] = "<redacted>"
			continue
		}
		out[strKey] = recurse(rv.MapIndex(k).Interface())
	}
	return out
}

func hookJSONableSeq(rv reflect.Value, recurse func(any) any, caps HookJSONCaps) any {
	n := rv.Len()
	limit := n
	if limit > caps.MaxSequence {
		limit = caps.MaxSequence
	}
	out := make([]any, 0, limit+1)
	for i := 0; i < limit; i++ {
		out = append(out, recurse(rv.Index(i).Interface()))
	}
	if n > caps.MaxSequence {
		out = append(out, map[string]any{"_truncated_items": n - caps.MaxSequence})
	}
	return out
}

// SanitizeHookPayload returns JSON safe data under the size cap, retrying with tighter caps then a preview
func SanitizeHookPayload(value any) any {
	limit := HookPayloadMaxChars()
	encoded := ""
	for _, caps := range []HookJSONCaps{DefaultHookJSONCaps, {MaxDepth: 8, MaxString: 1000, MaxSequence: 50}} {
		payload := HookJSONable(value, caps)
		raw, err := json.Marshal(payload)
		if err != nil {
			s, _ := truncateRunes(fmt.Sprint(payload), limit)
			return s
		}
		encoded = string(raw)
		if len([]rune(encoded)) <= limit {
			return payload
		}
	}
	preview, _ := truncateRunes(encoded, limit)
	return map[string]any{"_truncated": true, "original_type": typeName(value), "preview": preview}
}

// requestBody renders a request as the wire shaped body a plugin inspects
func requestBody(req *Request) map[string]any {
	body := map[string]any{}
	if req == nil {
		return body
	}
	body["model"] = req.Model
	body["messages"] = req.Messages
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = req.ToolChoice
	}
	if req.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.Reasoning != (Reasoning{}) {
		reasoning := map[string]any{}
		if req.Reasoning.Effort != "" {
			reasoning["effort"] = req.Reasoning.Effort
		}
		if req.Reasoning.BudgetTokens > 0 {
			reasoning["budget_tokens"] = req.Reasoning.BudgetTokens
		}
		if req.Reasoning.Enabled != nil {
			reasoning["enabled"] = *req.Reasoning.Enabled
		}
		if req.Reasoning.Summary != "" {
			reasoning["summary"] = req.Reasoning.Summary
		}
		if req.Reasoning.IncludeInResponse {
			reasoning["include_in_response"] = true
		}
		body["reasoning"] = reasoning
	}
	if req.ResponseFormat != nil {
		body["response_format"] = req.ResponseFormat
	}
	if req.ExtraBody != nil {
		body["extra_body"] = req.ExtraBody
	}
	body["stream"] = req.Stream
	if req.ServiceTier != "" {
		body["service_tier"] = req.ServiceTier
	}
	if len(req.Metadata) > 0 {
		body["metadata"] = req.Metadata
	}
	if req.User != "" {
		body["user"] = req.User
	}
	if req.CacheTTL != "" {
		body["cache_ttl"] = req.CacheTTL
	}
	if req.CacheScope != "" {
		body["cache_scope"] = req.CacheScope
	}
	return body
}

// RequestPayloadForHook is the sanitized POST body view of a request without transport only fields
func RequestPayloadForHook(req *Request) any {
	return SanitizeHookPayload(map[string]any{"method": "POST", "body": requestBody(req)})
}

// UsageSummaryForHook returns the token buckets of a response with hermes prompt and total sums
func UsageSummaryForHook(resp *Response) map[string]any {
	if resp == nil {
		return nil
	}
	u := resp.Usage
	if u == (Usage{}) {
		return nil
	}
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	return map[string]any{
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_tokens": u.CacheReadTokens, "cache_write_tokens": u.CacheWriteTokens,
		"reasoning_tokens": u.ReasoningTokens, "request_count": 1,
		"prompt_tokens": prompt, "total_tokens": prompt + u.OutputTokens,
	}
}

// ResponsePayloadForHook is the sanitized model, upstream provider, finish reason, assistant message, and usage view
func ResponsePayloadForHook(resp *Response, finishReason string) any {
	var model, upstream any
	var msg *state.Message
	if resp != nil {
		if resp.Model != "" {
			model = resp.Model
		}
		if resp.Provider != "" {
			upstream = resp.Provider
		}
		msg = resp.Message
	}
	assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}
	if msg != nil {
		if msg.Role != "" {
			assistant["role"] = msg.Role
		}
		assistant["content"] = msg.Content
		calls := make([]any, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, tc)
		}
		assistant["tool_calls"] = calls
	}
	var finish any
	if finishReason != "" {
		finish = finishReason
	}
	return SanitizeHookPayload(map[string]any{
		"model": model, "upstream_provider": upstream, "finish_reason": finish,
		"assistant_message": assistant, "usage": UsageSummaryForHook(resp),
	})
}

// InvokeAPIRequestErrorHook fires api_request_error with the duration measured at dispatch time
func InvokeAPIRequestErrorHook(ctx context.Context, hooks Hooks, p *ErrorPayload, req *Request) {
	if hooks == nil || p == nil || !hooks.Has(HookAPIRequestError) {
		return
	}
	p.EndedAt = time.Now()
	p.APIDuration = p.EndedAt.Sub(p.StartedAt)
	p.Request = RequestPayloadForHook(req)
	hooks.Error(ctx, p)
}
