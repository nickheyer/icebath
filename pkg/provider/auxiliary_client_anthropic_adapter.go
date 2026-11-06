package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Merges an OpenAI response format into the Anthropic output_config as a json_schema format
func auxTranslateAnthropicResponseFormat(anthropicKwargs map[string]any, responseFormat any) {
	rf, ok := responseFormat.(map[string]any)
	if !ok {
		return
	}
	var schema any
	switch rf["type"] {
	case "json_schema":
		jsonSchema, ok := rf["json_schema"].(map[string]any)
		if !ok {
			return
		}
		s, has := jsonSchema["schema"]
		if !has {
			return
		}
		schema = s
	case "json_object":
		schema = map[string]any{"type": "object"}
	default:
		return
	}
	outputConfig, ok := anthropicKwargs["output_config"].(map[string]any)
	if !ok {
		outputConfig = map[string]any{}
		anthropicKwargs["output_config"] = outputConfig
	}
	outputConfig["format"] = map[string]any{"type": "json_schema", "schema": schema}
}

// Messages create seam the tests swap to capture the translated api kwargs
var auxCreateAnthropicMessageFn = createAnthropicMessage

// Translates chat completions kwargs into one Anthropic Messages request
type auxAnthropicCompletionsAdapter struct {
	client  *anthropicSDKClient
	model   string
	isOAuth bool
	baseURL string
}

// Adapter whose base URL is the given one, else the client's own when it is the Nous portal
func newAuxAnthropicCompletionsAdapter(client *anthropicSDKClient, model string, isOAuth bool, baseURL string) *auxAnthropicCompletionsAdapter {
	a := &auxAnthropicCompletionsAdapter{client: client, model: model, isOAuth: isOAuth, baseURL: baseURL}
	if a.baseURL == "" && client != nil {
		if candidate := client.BaseURL(); candidate != "" && isNousPortalEndpoint(candidate) {
			a.baseURL = candidate
		}
	}
	return a
}

// Chat completions tool choice in the Anthropic vocabulary, nil when it has no equivalent
func auxAnthropicToolChoice(choice any) any {
	switch c := choice.(type) {
	case map[string]any:
		choiceType := strings.ToLower(pyStr(c["type"]))
		if choiceType == "function" {
			fn, _ := c["function"].(map[string]any)
			return fn["name"]
		}
		if choiceType == "auto" || choiceType == "required" || choiceType == "none" {
			return choiceType
		}
		return nil
	case string:
		return c
	}
	return nil
}

func (a *auxAnthropicCompletionsAdapter) create(ctx context.Context, kwargs map[string]any) (any, error) {
	model := a.model
	if v, ok := kwargs["model"]; ok {
		model = pyStr(v)
	}
	var maxTokens any
	if !pyrepr.Truthy(kwargs["_skip_zai_max_tokens"]) {
		maxTokens = kwargs["max_tokens"]
		if !pyrepr.Truthy(maxTokens) {
			maxTokens = kwargs["max_completion_tokens"]
		}
	}
	reasoningCfg := kwargs["_reasoning_config"]
	if reasoningCfg == nil {
		if eb, ok := kwargs["extra_body"].(map[string]any); ok {
			if rc, ok := eb["reasoning"].(map[string]any); ok {
				reasoningCfg = rc
			}
		}
	}
	messages, err := auxKwargDicts(kwargs, "messages", false)
	if err != nil {
		return nil, err
	}
	toolDefs, err := auxKwargDicts(kwargs, "tools", true)
	if err != nil {
		return nil, err
	}
	anthropicKwargs, err := anthBuildKwargs(anthBuildInput{
		model: model, messages: messages, tools: toolDefs, maxTokens: maxTokens,
		reasoning: anthReasoningFromRequest(reasoningFromConfig(reasoningCfg)), toolChoice: auxAnthropicToolChoice(kwargs["tool_choice"]),
		isOAuth: a.isOAuth, baseURL: a.baseURL,
	})
	if err != nil {
		return nil, err
	}
	if temperature := kwargs["temperature"]; temperature != nil && !anthForbidsSamplingParams(model) {
		anthropicKwargs["temperature"] = temperature
	}
	if headers, ok := auxDictKwarg(kwargs["extra_headers"]); ok && len(headers) > 0 {
		merged, _ := auxDictKwarg(anthropicKwargs["extra_headers"])
		if merged == nil {
			merged = map[string]any{}
		}
		for k, v := range headers {
			merged[k] = v
		}
		anthropicKwargs["extra_headers"] = merged
	}
	if rf := kwargs["response_format"]; rf != nil {
		auxTranslateAnthropicResponseFormat(anthropicKwargs, rf)
	}
	if callerExtra, ok := kwargs["extra_body"].(map[string]any); ok && len(callerExtra) > 0 {
		auxTranslateAnthropicResponseFormat(anthropicKwargs, callerExtra["response_format"])
		passthrough := map[string]any{}
		for k, v := range callerExtra {
			if k != "reasoning" && k != "response_format" && !strings.HasPrefix(k, "_") {
				passthrough[k] = v
			}
		}
		if len(passthrough) > 0 {
			existing, ok := anthropicKwargs["extra_body"].(map[string]any)
			merged := map[string]any{}
			if ok {
				for k, v := range existing {
					merged[k] = v
				}
			}
			for k, v := range passthrough {
				merged[k] = v
			}
			anthropicKwargs["extra_body"] = merged
		}
	}
	var hook func(event any) error
	if auxProgressActive(ctx) {
		hook = auxAnthropicAuxStreamEventHook(ctx)
	}
	response, err := auxCreateAnthropicMessageFn(ctx, a.client, anthropicKwargs, hook)
	if err != nil {
		return nil, err
	}
	normalized, err := anthropicMessagesTransport{}.NormalizeResponse(response, map[string]any{"strip_tool_prefix": a.isOAuth})
	if err != nil {
		return nil, err
	}
	var usage any
	if u, ok := response["usage"].(map[string]any); ok && len(u) > 0 {
		prompt, completion := kwargInt(u, "input_tokens"), kwargInt(u, "output_tokens")
		total := kwargInt(u, "total_tokens")
		if total == 0 {
			total = prompt + completion
		}
		usage = map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": total}
	}
	wire := state.MessageToDict(normalized.Message)
	message := map[string]any{"content": wire["content"], "tool_calls": wire["tool_calls"], "reasoning": wire["reasoning"]}
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": normalized.FinishReason}},
		"model":   model,
		"usage":   usage,
	}, nil
}

// OpenAI client compatible wrapper over a native Anthropic client
type AnthropicAuxiliaryClient struct {
	realClient *anthropicSDKClient
	adapter    *auxAnthropicCompletionsAdapter
	apiKey     any
	baseURL    string
}

// Wraps an already built Anthropic SDK client
func newAnthropicAuxiliaryClientFrom(realClient *anthropicSDKClient, model string, apiKey any, baseURL string, isOAuth bool) *AnthropicAuxiliaryClient {
	return &AnthropicAuxiliaryClient{realClient: realClient, adapter: newAuxAnthropicCompletionsAdapter(realClient, model, isOAuth, baseURL), apiKey: apiKey, baseURL: baseURL}
}

// Builds the Anthropic SDK client for the key and base URL and wraps it
func NewAnthropicAuxiliaryClient(ctx context.Context, model string, apiKey any, baseURL string, isOAuth bool) (*AnthropicAuxiliaryClient, error) {
	real, err := buildAnthropicClient(ctx, apiKey, baseURL, 0, false)
	if err != nil {
		return nil, err
	}
	return newAnthropicAuxiliaryClientFrom(real, model, apiKey, baseURL, isOAuth), nil
}

func (c *AnthropicAuxiliaryClient) BaseURL() string { return c.baseURL }

// Static key, empty for a callable bearer provider
func (c *AnthropicAuxiliaryClient) APIKey() string {
	key, _ := c.apiKey.(string)
	return key
}

// Runs the request through the Anthropic adapter
func (c *AnthropicAuxiliaryClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.adapter.create(ctx, kwargs)
}

// Closes the wrapped SDK client when it has one
func (c *AnthropicAuxiliaryClient) Close() error {
	if c.realClient == nil {
		return nil
	}
	return c.realClient.Close()
}

// Async twin of the Anthropic auxiliary client
type AsyncAnthropicAuxiliaryClient struct{ auxAsyncAuxiliaryClientBase }

// Wraps a sync Anthropic client for async callers
func NewAsyncAnthropicAuxiliaryClient(sync *AnthropicAuxiliaryClient) *AsyncAnthropicAuxiliaryClient {
	return &AsyncAnthropicAuxiliaryClient{newAuxAsyncAuxiliaryClientBase(sync)}
}

// Translates chat completions kwargs into one Bedrock Converse call
type auxBedrockCompletionsAdapter struct {
	region string
	model  string
}

// Float pointer of a numeric kwargs value, nil when absent or None
func auxKwargFloatPtr(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case float32:
		f := float64(n)
		return &f
	case int:
		f := float64(n)
		return &f
	case int64:
		f := float64(n)
		return &f
	}
	return nil
}

func (a *auxBedrockCompletionsAdapter) create(ctx context.Context, kwargs map[string]any) (any, error) {
	model := a.model
	if v, ok := kwargs["model"]; ok {
		model = pyStr(v)
	}
	maxTokens := kwargs["max_tokens"]
	if !pyrepr.Truthy(maxTokens) {
		maxTokens = kwargs["max_completion_tokens"]
	}
	var stop []string
	switch s := kwargs["stop"].(type) {
	case string:
		stop = []string{s}
	case []string:
		stop = s
	case []any:
		for _, item := range s {
			stop = append(stop, pyStr(item))
		}
	}
	if choice := kwargs["tool_choice"]; choice != nil {
		slog.Debug(fmt.Sprintf("BedrockAuxiliaryClient: tool_choice=%s not supported by the Converse shim — ignored.", pyrepr.Repr(choice)))
	}
	if pyrepr.Truthy(kwargs["stream"]) {
		slog.Debug(fmt.Sprintf("BedrockAuxiliaryClient: stream=True requested for %s — returning a complete response (Converse shim does not stream); caller downgrades to non-streaming.", model))
	}
	messages, err := auxKwargDicts(kwargs, "messages", false)
	if err != nil {
		return nil, err
	}
	toolDefs, err := auxKwargDicts(kwargs, "tools", true)
	if err != nil {
		return nil, err
	}
	in := bedrockConverseInput{Model: model, Messages: messages, Tools: toolDefs,
		Temperature: auxKwargFloatPtr(kwargs["temperature"]), TopP: auxKwargFloatPtr(kwargs["top_p"]), StopSequences: stop}
	if pyrepr.Truthy(maxTokens) {
		v, err := auxPyIntCall(maxTokens)
		if err != nil {
			return nil, err
		}
		in.MaxTokens = &v
	}
	result, err := callBedrockConverse(ctx, a.region, in)
	if err != nil {
		return nil, err
	}
	auxNotifyAuxProviderResponse(ctx)
	return auxBedrockResultWire(result), nil
}

// Python int() of a kwargs value, floats truncated and text parsed, ValueError for anything else
func auxPyIntCall(v any) (int, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case float64:
		if math.IsNaN(x) {
			return 0, &ValueError{Message: "cannot convert float NaN to integer"}
		}
		if math.IsInf(x, 0) {
			return 0, &ValueError{Message: "cannot convert float infinity to integer"}
		}
		return int(x), nil
	case float32:
		return auxPyIntCall(float64(x))
	case string:
		if n, ok := pyIntText(x); ok {
			return int(n), nil
		}
		return 0, &ValueError{Message: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(x))}
	}
	if n, ok := pyrepr.Int(v); ok {
		return int(n.Int64()), nil
	}
	return 0, &ValueError{Message: fmt.Sprintf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyrepr.TypeName(v))}
}

// OpenAI chat completion shape of a Converse result with cache tokens folded into the prompt count
func auxBedrockResultWire(r *bedrockConverseResult) map[string]any {
	message := map[string]any{"role": "assistant", "content": r.Content, "tool_calls": nil, "reasoning_details": nil,
		"reasoning_content": nil, "bedrock_content_blocks": nil}
	if len(r.ToolCalls) > 0 {
		calls := make([]any, 0, len(r.ToolCalls))
		for _, tc := range r.ToolCalls {
			calls = append(calls, state.ToolCallToDict(tc))
		}
		message["tool_calls"] = calls
	}
	if len(r.ReasoningDetails) > 0 {
		message["reasoning_details"] = r.ReasoningDetails
	}
	if r.ReasoningContent != "" {
		message["reasoning_content"] = r.ReasoningContent
	}
	if len(r.ContentBlocks) > 0 {
		message["bedrock_content_blocks"] = r.ContentBlocks
	}
	usage := map[string]any{"prompt_tokens": r.Usage.PromptTokens, "completion_tokens": r.Usage.CompletionTokens,
		"total_tokens": r.Usage.TotalTokens, "cache_read_input_tokens": r.Usage.CacheReadInputTokens,
		"cache_creation_input_tokens": r.Usage.CacheCreationInputTokens}
	return map[string]any{
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": r.FinishReason}},
		"usage":   usage, "model": r.Model,
	}
}

// OpenAI client compatible wrapper over the Bedrock Converse API
type BedrockAuxiliaryClient struct {
	region  string
	model   string
	adapter *auxBedrockCompletionsAdapter
}

// Wraps Converse for the region and model
func NewBedrockAuxiliaryClient(region, model string) *BedrockAuxiliaryClient {
	return &BedrockAuxiliaryClient{region: region, model: model, adapter: &auxBedrockCompletionsAdapter{region: region, model: model}}
}

func (c *BedrockAuxiliaryClient) BaseURL() string {
	return "https://bedrock-runtime." + c.region + ".amazonaws.com"
}
func (c *BedrockAuxiliaryClient) APIKey() string { return "aws-sdk" }

// Runs the request through the Converse adapter
func (c *BedrockAuxiliaryClient) CreateChatCompletion(ctx context.Context, kwargs map[string]any) (any, error) {
	return c.adapter.create(ctx, kwargs)
}

// Converse holds no client side transport so closing releases nothing
func (c *BedrockAuxiliaryClient) Close() error { return nil }

// Async twin of the Bedrock auxiliary client
type AsyncBedrockAuxiliaryClient struct{ auxAsyncAuxiliaryClientBase }

// Wraps a sync Bedrock client for async callers
func NewAsyncBedrockAuxiliaryClient(sync *BedrockAuxiliaryClient) *AsyncBedrockAuxiliaryClient {
	return &AsyncBedrockAuxiliaryClient{newAuxAsyncAuxiliaryClientBase(sync)}
}

// Whether the base URL speaks Anthropic Messages, an anthropic path, api.anthropic.com or the Kimi coding host
func auxEndpointSpeaksAnthropicMessages(baseURL string) (bool, error) {
	normalized := strings.TrimRight(strings.ToLower(pyStripSpace(baseURL)), "/")
	if normalized == "" {
		return false, nil
	}
	parsed, err := pyURLParse(normalized, "")
	if err != nil {
		return false, err
	}
	if path := strings.TrimRight(parsed.path, "/"); strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") {
		return true, nil
	}
	hostname := baseURLHostname(normalized)
	return hostname == "api.anthropic.com" || (hostname == "api.kimi.com" && strings.Contains(normalized, "/coding")), nil
}

// Rewraps a plain OpenAI wire client as an Anthropic client when the endpoint or explicit api_mode speaks Messages
func auxMaybeWrapAnthropic(ctx context.Context, client AuxClient, model string, apiKey any, baseURL, apiMode string) (AuxClient, error) {
	switch client.(type) {
	case *auxProbeClient, *AnthropicAuxiliaryClient, *BedrockAuxiliaryClient, *CodexAuxiliaryClient:
		return client, nil
	}
	if auxClientDeclares(client, "ICEBATH_SKIP_TRANSPORT_WRAP") {
		return client, nil
	}
	if apiMode != "anthropic_messages" {
		if apiMode != "" {
			return client, nil
		}
		speaks, err := auxEndpointSpeaksAnthropicMessages(baseURL)
		if err != nil {
			return nil, err
		}
		if !speaks {
			return client, nil
		}
	}
	real, err := buildAnthropicClient(ctx, apiKey, baseURL, 0, false)
	if err != nil {
		slog.Warn(fmt.Sprintf("Failed to build Anthropic client for %s (%v) — falling back to OpenAI-wire client.", baseURL, err))
		return client, nil
	}
	shown, mode := baseURL, apiMode
	if len(shown) > 60 {
		shown = string([]rune(shown)[:min(60, len([]rune(shown)))])
	}
	if mode == "" {
		mode = "auto-detected"
	}
	slog.Debug(fmt.Sprintf("Auxiliary transport: wrapping client in AnthropicAuxiliaryClient (model=%s, base_url=%s, api_mode=%s)", model, shown, mode))
	return newAnthropicAuxiliaryClientFrom(real, model, apiKey, baseURL, false), nil
}
