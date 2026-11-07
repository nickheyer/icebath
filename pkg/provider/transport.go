package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/state"
)

// Transport is the data path of one api mode, messages and tools in, provider kwargs out, raw responses normalized back
type Transport interface {
	APIMode() string
	ConvertMessages(messages []map[string]any, kwargs map[string]any) (any, error)
	ConvertTools(tools []map[string]any) any
	BuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error)
	NormalizeResponse(response any, kwargs map[string]any) (*Response, error)
	ValidateResponse(response any) bool
	ExtractCacheStats(response any) map[string]int
	MapFinishReason(raw string) string
}

var (
	transportsMu sync.RWMutex
	transports   = map[string]func() Transport{}
)

// RegisterTransport binds an api mode to the constructor of its transport, the seam a provider plugin dialect uses
func RegisterTransport(apiMode string, newTransport func() Transport) {
	transportsMu.Lock()
	defer transportsMu.Unlock()
	transports[apiMode] = newTransport
}

// GetTransport returns a fresh transport for the api mode, nil when no transport is registered for it
func GetTransport(apiMode string) Transport {
	transportsMu.RLock()
	newTransport := transports[apiMode]
	transportsMu.RUnlock()
	if newTransport == nil {
		return nil
	}
	return newTransport()
}

// RegisteredAPIModes lists every api mode with a registered transport, built in and plugin supplied
func RegisteredAPIModes() map[string]bool {
	transportsMu.RLock()
	defer transportsMu.RUnlock()
	out := make(map[string]bool, len(transports))
	for mode := range transports {
		out[mode] = true
	}
	return out
}

func init() {
	RegisterTransport(string(ModeAnthropicMessages), func() Transport { return &anthropicMessagesTransport{} })
	RegisterTransport(string(ModeCodexResponses), func() Transport { return &responsesAPITransport{codex: newCodexTransport()} })
	RegisterTransport(string(ModeChatCompletions), func() Transport { return &chatCompletionsTransport{} })
	RegisterTransport(string(modeBedrockConverse), func() Transport { return &bedrockConverseTransport{} })
}

// Stop reason table lookup with stop for unknown reasons, passthrough when the transport has no table
func mapStopReason(table map[string]string, raw string) string {
	if table == nil {
		return raw
	}
	if v, ok := table[raw]; ok {
		return v
	}
	return "stop"
}

func kwargString(kwargs map[string]any, key string) string {
	s, _ := kwargs[key].(string)
	return s
}

func kwargBool(kwargs map[string]any, key string, def bool) bool {
	v, ok := kwargs[key]
	if !ok {
		return def
	}
	return pyTruthy(v)
}

func kwargInt(kwargs map[string]any, key string) int {
	switch v := kwargs[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

// Reasoning config dict of the agent as the provider neutral reasoning controls
func reasoningFromConfig(v any) Reasoning {
	cfg, _ := v.(map[string]any)
	var r Reasoning
	if cfg == nil {
		return r
	}
	if enabled, ok := cfg["enabled"].(bool); ok {
		r.Enabled = &enabled
	}
	r.Effort, _ = cfg["effort"].(string)
	r.BudgetTokens = kwargInt(cfg, "budget_tokens")
	return r
}

// Live wire messages of OpenAI shaped dicts
func messagesFromDicts(messages []map[string]any) []*state.Message {
	out := make([]*state.Message, 0, len(messages))
	for _, m := range messages {
		out = append(out, state.MessageFromDict(m))
	}
	return out
}

type anthropicMessagesTransport struct{}

func (anthropicMessagesTransport) APIMode() string { return string(ModeAnthropicMessages) }

// System param and Anthropic messages, base_url and model picking the thinking replay policy
func (anthropicMessagesTransport) ConvertMessages(messages []map[string]any, kwargs map[string]any) (any, error) {
	system, converted := convertMessagesToAnthropic(messages, convertOptions{baseURL: kwargString(kwargs, "base_url"), model: kwargString(kwargs, "model")})
	return []any{system, converted}, nil
}

func (anthropicMessagesTransport) ConvertTools(tools []map[string]any) any {
	return convertToolsToAnthropic(tools)
}

// Messages create kwargs with the transport defaults for every build parameter the caller omits
func (anthropicMessagesTransport) BuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error) {
	maxTokens, ok := params["max_tokens"]
	if !ok {
		maxTokens = 16384
	}
	return anthBuildKwargs(anthBuildInput{
		model: model, messages: messages, tools: tools, maxTokens: maxTokens,
		reasoning: anthReasoningFromRequest(reasoningFromConfig(params["reasoning_config"])), toolChoice: params["tool_choice"],
		isOAuth: kwargBool(params, "is_oauth", false), preserveDots: kwargBool(params, "preserve_dots", false),
		contextLength: kwargInt(params, "context_length"), baseURL: kwargString(params, "base_url"),
		fastMode: kwargBool(params, "fast_mode", false), dropContext1M: kwargBool(params, "drop_context_1m_beta", false),
	})
}

// Normalized raw Messages response, guardrail substitutions as content_filter and OAuth tool names unprefixed on request
func (anthropicMessagesTransport) NormalizeResponse(response any, kwargs map[string]any) (*Response, error) {
	message, _ := response.(map[string]any)
	c := &anthropicClient{oauthWire: kwargBool(kwargs, "strip_tool_prefix", false), transport: &anthBedrockTransport{}}
	return c.normalizeResponse(message, 200, nil, 0)
}

func (anthropicMessagesTransport) ValidateResponse(response any) bool {
	message, ok := response.(map[string]any)
	return ok && anthValidateMessage(message)
}

// Anthropic cache read and cache creation token counts, nil when neither is set
func (anthropicMessagesTransport) ExtractCacheStats(response any) map[string]int {
	message, _ := response.(map[string]any)
	usage, ok := message["usage"].(map[string]any)
	if !ok {
		return nil
	}
	cached, written := kwargInt(usage, "cache_read_input_tokens"), kwargInt(usage, "cache_creation_input_tokens")
	if cached == 0 && written == 0 {
		return nil
	}
	return map[string]int{"cached_tokens": cached, "creation_tokens": written}
}

func (anthropicMessagesTransport) MapFinishReason(raw string) string {
	return anthMapFinishReason(raw)
}

type bedrockConverseTransport struct{ model string }

func (*bedrockConverseTransport) APIMode() string { return string(modeBedrockConverse) }

// Converse system blocks and messages for the model named in kwargs
func (*bedrockConverseTransport) ConvertMessages(messages []map[string]any, kwargs map[string]any) (any, error) {
	system, converted := convertMessagesToConverse(messages, nil, kwargString(kwargs, "model"))
	return []any{system, converted}, nil
}

func (*bedrockConverseTransport) ConvertTools(tools []map[string]any) any {
	return convertToolsToConverse(tools)
}

// Converse kwargs with the dispatch sentinels, remembering the model that stamps normalized reasoning
func (t *bedrockConverseTransport) BuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error) {
	t.model = model
	in := bedrockConverseInput{Model: model, Messages: messages, Tools: tools,
		Reasoning: anthReasoningFromRequest(reasoningFromConfig(params["reasoning_config"]))}
	if _, ok := params["max_tokens"]; ok && params["max_tokens"] != nil {
		n := kwargInt(params, "max_tokens")
		in.MaxTokens = &n
	}
	if temp, ok := params["temperature"].(float64); ok {
		in.Temperature = &temp
	}
	in.GuardrailConfig, _ = params["guardrail_config"].(map[string]any)
	kwargs := buildBedrockConverseKwargs(in)
	kwargs["__bedrock_converse__"] = true
	region, ok := params["region"]
	if !ok {
		region = "us-east-1"
	}
	kwargs["__bedrock_region__"] = region
	return kwargs, nil
}

func (t *bedrockConverseTransport) NormalizeResponse(response any, kwargs map[string]any) (*Response, error) {
	return bedrockTransportNormalize(response, t.model)
}

func (*bedrockConverseTransport) ValidateResponse(response any) bool {
	return validateBedrockConverseResponse(response)
}

func (*bedrockConverseTransport) ExtractCacheStats(response any) map[string]int { return nil }

func (*bedrockConverseTransport) MapFinishReason(raw string) string {
	return bedrockTransportFinishReason(raw)
}

var responsesStopReasonMap = map[string]string{"completed": "stop", "incomplete": "length", "failed": "stop", "cancelled": "stop"}

type responsesAPITransport struct{ codex *codexTransport }

func (*responsesAPITransport) APIMode() string { return string(ModeCodexResponses) }

// Responses params of the agent kwargs, every endpoint quirk resolved from base_url
func codexParamsFromKwargs(params map[string]any) codexParams {
	ctx := context.Background()
	p := codexParams{
		ctx: ctx, instructions: kwargString(params, "instructions"), reasoning: reasoningFromRequest(reasoningFromConfig(params["reasoning_config"])),
		sessionID: kwargString(params, "session_id"), cacheScopeID: kwargString(params, "cache_scope_id"),
		maxTokens: kwargInt(params, "max_tokens"), provider: kwargString(params, "provider"), baseURL: kwargString(params, "base_url"),
		isCodexBackend: params["is_codex_backend"] == true, isXAI: params["is_xai_responses"] == true,
		replayEncryptedReasoning: kwargBool(params, "replay_encrypted_reasoning", true), textVerbosity: kwargString(params, "text_verbosity"),
		webSearchBackend: func() (string, error) { return activeWebSearchBackend(ctx) },
	}
	if timeout, ok := params["timeout"].(float64); ok {
		p.timeout = timeout
	}
	p.requestOverrides, _ = params["request_overrides"].(map[string]any)
	p.profileReasoning, _ = params["profile_reasoning"].(map[string]any)
	if list, ok := params["context_management"].([]map[string]any); ok {
		p.contextManagement = list
	}
	return p
}

// Responses input items of the messages under the endpoint quirks of base_url
func (t *responsesAPITransport) ConvertMessages(messages []map[string]any, kwargs map[string]any) (any, error) {
	p := codexParamsFromKwargs(kwargs)
	endpoint := responsesEndpointQuirksFor(p.baseURL)
	return t.codex.convertMessages(codexMsgsOf(messagesFromDicts(messages)), p, &endpoint, kwargString(kwargs, "model"))
}

func (*responsesAPITransport) ConvertTools(tools []map[string]any) any {
	return responsesTools(tools)
}

func (t *responsesAPITransport) BuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error) {
	return t.codex.buildKwargs(model, messagesFromDicts(messages), tools, codexParamsFromKwargs(params))
}

// Normalized assistant turn of a Responses payload stamped with the issuer kind and model in kwargs
func (t *responsesAPITransport) NormalizeResponse(response any, kwargs map[string]any) (*Response, error) {
	payload, _ := response.(map[string]any)
	msg, finish, err := t.codex.normalizeResponse(payload, kwargString(kwargs, "issuer_kind"), kwargString(kwargs, "issuer_model"))
	if err != nil {
		return nil, err
	}
	return &Response{Message: msg, FinishReason: finish, Usage: codexUsageFromMap(asMapOrNil(payload["usage"]))}, nil
}

func (*responsesAPITransport) ValidateResponse(response any) bool {
	payload, ok := response.(map[string]any)
	return ok && validateCodexResponse(payload)
}

func (*responsesAPITransport) ExtractCacheStats(response any) map[string]int { return nil }

func (*responsesAPITransport) MapFinishReason(raw string) string {
	return mapStopReason(responsesStopReasonMap, raw)
}

// Responses transport preflight of outgoing kwargs, an AttributeError for every transport lacking it
func PreflightKwargs(t Transport, apiKwargs map[string]any, allowStream, dropReplayItemIDs, sanitizeHarmony bool) (map[string]any, error) {
	if rt, ok := t.(*responsesAPITransport); ok && rt.codex != nil {
		return rt.codex.preflightKwargs(apiKwargs, allowStream, dropReplayItemIDs, sanitizeHarmony)
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'preflight_kwargs'", transportClassName(t))
}

// Python class name of a transport for the AttributeError text
func transportClassName(t Transport) string {
	switch t.(type) {
	case *chatCompletionsTransport:
		return "ChatCompletionsTransport"
	case *anthropicMessagesTransport:
		return "AnthropicTransport"
	case *bedrockConverseTransport:
		return "BedrockTransport"
	case *responsesAPITransport:
		return "ResponsesApiTransport"
	case nil:
		return "NoneType"
	}
	return fmt.Sprintf("%T", t)
}

type chatCompletionsTransport struct{}

func (chatCompletionsTransport) APIMode() string { return string(ModeChatCompletions) }

// Messages with the internal fields strict chat completions providers reject stripped, the input when nothing changes
func (chatCompletionsTransport) ConvertMessages(messages []map[string]any, kwargs map[string]any) (any, error) {
	return chatCompletionsSanitizeMessages(messages, kwargs), nil
}

func (chatCompletionsTransport) ConvertTools(tools []map[string]any) any { return tools }

func (chatCompletionsTransport) BuildKwargs(model string, messages []map[string]any, tools []map[string]any, params map[string]any) (map[string]any, error) {
	return chatCompletionsBuildKwargs(model, messages, tools, params)
}

func (chatCompletionsTransport) NormalizeResponse(response any, kwargs map[string]any) (*Response, error) {
	payload, _ := response.(map[string]any)
	c := &chatCompletionsClient{route: Route{Provider: kwargString(kwargs, "provider"), Model: kwargString(kwargs, "model"), BaseURL: kwargString(kwargs, "base_url")}}
	return c.normalizeResponse(payload, 200, nil, 0)
}

// True when the response has choices and is not a router failure shim
func (chatCompletionsTransport) ValidateResponse(response any) bool {
	payload, _ := response.(map[string]any)
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	return !chatResponseIsRouterTimeoutShim(payload)
}

// Cache stats from prompt_tokens_details or DeepSeek prompt_cache_hit_tokens, nil when neither is set
func (chatCompletionsTransport) ExtractCacheStats(response any) map[string]int {
	payload, _ := response.(map[string]any)
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		return nil
	}
	cached, written := 0, 0
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached, written = kwargInt(details, "cached_tokens"), kwargInt(details, "cache_write_tokens")
	}
	if cached == 0 {
		cached = kwargInt(usage, "prompt_cache_hit_tokens")
	}
	if cached == 0 && written == 0 {
		return nil
	}
	return map[string]int{"cached_tokens": cached, "creation_tokens": written}
}

func (chatCompletionsTransport) MapFinishReason(raw string) string { return raw }

// True when the first choice is the empty router failure shim some gateways send in place of an error
func chatResponseIsRouterTimeoutShim(payload map[string]any) bool {
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content, _ := message["content"].(string)
	calls, _ := message["tool_calls"].([]any)
	return isRouterTimeoutShim(content, len(calls), ccUsageFromMap(asMapOrNil(payload["usage"])))
}

func asMapOrNil(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
