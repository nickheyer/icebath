package transport

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// One request shaped for the wire: its body, address and headers, what its hooks were told, and what its answer is read with
type prepared struct {
	request *provider.Request
	call    *provider.Call
	spec    provider.Spec
	// Kind claiming the address, which may differ from the route's for a provider of your own at a known host
	hosted provider.Spec
	body   map[string]any
	url    string
	header http.Header
	tokens int
	// Names the tools were sent under, mapped to their own
	aliases map[string]string
	limits  transport.Limits
	refused transport.Refused
	// Fields the kinds declare their replies carry beside the text, opaque ones and those holding reasoning
	opaque, texts []string
	llamacpp      bool
	nativeGemini  bool
	settings      config.View
}

// Kind of the route as its provider has it, its settings resolved, else as its plugin registers it
func (c *client) kind(cfg *config.Config) provider.Spec {
	if instance, ok := c.r.providers.Instance(cfg, c.route.Provider); ok && instance.ID == c.route.Provider && instance.Spec.Name == c.route.Spec {
		return instance.Spec
	}
	spec, _ := c.r.providers.Spec(c.route.Spec)
	return spec
}

// Names a llama.cpp server goes by as a provider
var llamacppNames = []string{"llamacpp", "llama.cpp", "llama-cpp"}

func (c *client) prepare(ctx context.Context, request *provider.Request) (*prepared, error) {
	cfg := cmp.Or(request.Config, c.r.config)
	spec := c.kind(cfg)
	settings := cfg.View(providerconfig.Key).View(c.route.Provider)
	hosted := spec
	if claimed := c.r.models.Kind(c.route.BaseURL); claimed != "" && claimed != spec.Name {
		hosted, _ = c.r.providers.Spec(claimed)
	}
	local := c.route.Local || models.Local(c.route.BaseURL)
	p := &prepared{request: request, spec: spec, hosted: hosted, nativeGemini: provider.NativeGemini(c.route.BaseURL), settings: settings}
	p.llamacpp = slices.Contains(llamacppNames, strings.ToLower(c.route.Provider)) ||
		local && c.r.models.ServerType(ctx, cfg, c.route.BaseURL, c.route.APIKey) == "llamacpp"
	call := &provider.Call{Context: ctx, Route: c.route, Model: request.Model, Settings: settings, Config: cfg, SessionID: request.SessionID, Source: request.Source,
		Scope: transport.Scope(request.SessionID), Images: images(request.Messages), UserTurn: request.UserTurn, Kind: c.r.providers.Spec}
	call.OllamaThinks = func() bool {
		thinks, _ := c.r.models.OllamaThinks(ctx, cfg, request.Model, c.route.BaseURL, c.route.APIKey)
		return thinks
	}
	call.Window = func() int {
		if !local {
			return 0
		}
		window, ok := c.r.models.OllamaWindow(ctx, cfg, request.Model, c.route.BaseURL, c.route.APIKey)
		if !ok {
			return 0
		}
		if limit := settings.GetInt(providerconfig.ContextLength); limit > 0 && window > limit {
			window = limit
		}
		return window
	}
	p.call = call
	call.Ask = request.Reasoning.Ask()
	if !call.Ask.Set {
		call.Ask = c.defaulted(cfg, spec, call)
	}
	call.Ask = call.Ask.Clamped(provider.Efforts)
	call.Thinking, _ = c.r.models.Thinking(ctx, cfg, c.route)
	for _, s := range c.r.providers.Specs() {
		if s.MessageCarrierOpaque != "" && !slices.Contains(p.opaque, s.MessageCarrierOpaque) {
			p.opaque = append(p.opaque, s.MessageCarrierOpaque)
		}
		if s.MessageCarrierText != "" && !slices.Contains(p.texts, s.MessageCarrierText) {
			p.texts = append(p.texts, s.MessageCarrierText)
		}
	}

	caching := c.r.registry.Caching(cfg, request, c.route, spec)
	messages, planned := caching.Plan(c.messages(p, cfg), request.Tools)
	if spec.Messages != nil {
		messages = spec.Messages(call, messages)
	}
	developer(messages, request.Model)
	tools, aliases := c.tools(p, planned)
	p.aliases = aliases
	body := map[string]any{"model": request.Model, "messages": messages}
	if temperature := spec.Temperature(request.Model, request.Temperature); temperature != nil {
		body["temperature"] = *temperature
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	limit := request.MaxTokens
	if spec.OutputCap != nil {
		limit = spec.OutputCap(call, limit)
	} else if limit <= 0 {
		limit = spec.DefaultMaxTokens
	}
	if limit > 0 {
		body[c.capField(spec, request.Model)] = raised(request.Model, call.Ask, limit)
	}
	extra, top := map[string]any{}, map[string]any{}
	if spec.ExtraBody != nil {
		maps.Copy(extra, spec.ExtraBody(call))
	}
	if spec.Shape != nil {
		shaped, over := spec.Shape(call)
		maps.Copy(extra, shaped)
		maps.Copy(top, over)
	}
	headers, _ := top["extra_headers"].(map[string]string)
	delete(top, "extra_headers")
	maps.Copy(body, top)
	if format := request.Format; format != nil && !slices.Contains(spec.UnsupportedResponseFormats, "json_schema") {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": format.Name, "schema": format.Schema,
			"strict": format.Strict}}
	}
	if nested, ok := request.Extra["extra_body"].(map[string]any); ok {
		maps.Copy(extra, nested)
	}
	if p.nativeGemini {
		maps.DeleteFunc(extra, func(key string, _ any) bool {
			return key != "thinking_config" && key != "thinkingConfig" && key != "max_output_tokens"
		})
	}
	maps.Copy(body, extra)
	for key, value := range request.Extra {
		if _, nested := value.(map[string]any); key != "extra_body" || !nested {
			body[key] = value
		}
	}
	if key, ok := body["prompt_cache_key"]; ok {
		if b := transport.BoundedKey(cast.ToString(key)); b != "" {
			body["prompt_cache_key"] = b
		} else {
			delete(body, "prompt_cache_key")
		}
	} else if spec.SupportsPromptCacheKey || provider.BaseHost(c.route.BaseURL) == "api.openai.com" {
		if key := transport.CacheKey(instructions(messages), tools, call.Scope); key != "" {
			body["prompt_cache_key"] = key
		}
	}
	p.body = body
	p.tokens = transport.Tokens(body)

	address, err := transport.Endpoint(c.route.BaseURL, "/chat/completions")
	if err != nil {
		return nil, err
	}
	p.url = address
	p.header = c.headers(p, cfg, settings, headers)
	p.limits = transport.Limits{Config: cfg, Settings: settings, Model: request.Model, Local: local, Tokens: p.tokens, Deadline: request.Deadline}
	p.refused = transport.Refused{Route: c.route, Model: request.Model, Spec: spec, Classify: request.Classify}
	return p, nil
}

// Output cap of a Gemini model raised to its published ceiling when its thinking counts against the cap
func raised(model string, ask provider.Ask, limit int) int {
	config := provider.GeminiThinking(model, ask)
	budget, budgeted := config["thinkingBudget"].(int)
	if _, level := config["thinkingLevel"]; len(config) == 0 || budgeted && budget <= 0 && !level {
		return limit
	}
	return max(limit, provider.GeminiMaxOutput)
}

// Reasoning a request that asks none is sent with: the kind's default, unless models.dev knows the model doesn't reason
func (c *client) defaulted(cfg *config.Config, spec provider.Spec, call *provider.Call) provider.Ask {
	if spec.DefaultReasoning == nil {
		return provider.Ask{}
	}
	ask := spec.DefaultReasoning(call)
	if !ask.Set {
		return ask
	}
	if caps, ok := c.r.models.Capabilities(cfg, c.route, false); ok && caps.Reasoning != nil && !*caps.Reasoning {
		return provider.Ask{}
	}
	return ask
}

// Whether the request carries pictures
func images(messages []*sessionsv1.Message) bool {
	return slices.ContainsFunc(messages, func(m *sessionsv1.Message) bool {
		return slices.ContainsFunc(m.GetMedia(), func(media *sessionsv1.Media) bool { return strings.HasPrefix(media.GetMimeType(), "image/") })
	})
}

// Model names whose system prompt goes under the developer role
var developerModels = []string{"gpt-5", "codex"}

// The first message as the developer's rather than the system's, for the models that take it so
func developer(messages []map[string]any, model string) {
	if len(messages) == 0 || messages[0]["role"] != "system" || !provider.Contains(strings.ToLower(model), developerModels...) {
		return
	}
	first := maps.Clone(messages[0])
	first["role"] = "developer"
	messages[0] = first
}

// Text of the leading system or developer message, which prompt caches are keyed on
func instructions(messages []map[string]any) string {
	if len(messages) == 0 || messages[0]["role"] != "system" && messages[0]["role"] != "developer" {
		return ""
	}
	if text, ok := messages[0]["content"].(string); ok {
		return text
	}
	raw, _ := json.Marshal(messages[0]["content"])
	return string(raw)
}

// Families of OpenAI models that refuse max_tokens and take max_completion_tokens
var completionCapModels = []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"}

// Field the output cap goes in: max_completion_tokens on OpenAI's and Azure's own APIs, else the kind's choice, else by the model's family
func (c *client) capField(spec provider.Spec, model string) string {
	if provider.BaseHost(c.route.BaseURL) == "api.openai.com" || provider.BaseHostMatches(c.route.BaseURL, "openai.azure.com") {
		return "max_completion_tokens"
	}
	if spec.OutputField != nil {
		if field := spec.OutputField(c.route.BaseURL, model); field != "" {
			return field
		}
	}
	if provider.HasPrefix(provider.BareModel(model), completionCapModels...) {
		return "max_completion_tokens"
	}
	return "max_tokens"
}

// Hosts that take an Azure api-key header beside the bearer token
var azureHosts = []string{"openai.azure.com", "cognitiveservices.azure.com", "services.ai.azure.com"}

// Headers of the request: icebath's user agent, the route's own, those of the kind and of the kind claiming its address, the extra headers
// of the transports and of the provider, the session's affinity header, then its credential
func (c *client) headers(p *prepared, cfg *config.Config, settings config.View, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", provider.UserAgent("IcebathAgent"))
	for name, value := range c.route.Headers {
		h.Set(name, value)
	}
	if p.spec.RequestHeaders != nil {
		for name, value := range p.spec.RequestHeaders(p.call) {
			h.Set(name, value)
		}
	}
	if p.hosted.Name != p.spec.Name && p.hosted.RequestHeaders != nil {
		for name, value := range p.hosted.RequestHeaders(p.call) {
			h.Set(name, value)
		}
	}
	for name, value := range extra {
		h.Set(name, value)
	}
	transport.Extra(h, cfg, settings, p.request.SessionID, true)
	if key := strings.TrimSpace(c.route.APIKey); key != "" && c.route.Sign == nil {
		h.Set("Authorization", "Bearer "+key)
		if slices.ContainsFunc(azureHosts, func(host string) bool { return provider.BaseHostMatches(c.route.BaseURL, host) }) {
			h.Set("api-key", key)
		}
	}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/event-stream")
	return h
}
