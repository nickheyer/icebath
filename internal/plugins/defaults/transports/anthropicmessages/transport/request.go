package transport

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	anthropicconfig "github.com/nickheyer/icebath/internal/plugins/defaults/transports/anthropicmessages/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Carriers the converse wire keeps its replies under, which Claude turns made through it are replayed from
const converseCarrier = string(provider.ModeBedrockConverse)

// Record of the model a Bedrock Converse turn was made by, among the turn's details
const issuerRecord = "bedrock.native_assistant"

// What a reply carried beside its text and calls that is replayed with it: its thinking blocks, and every block in its order when signed
// thinking came with a tool call
type carried struct {
	Details []any `json:"details,omitempty"`
	Blocks  []any `json:"blocks,omitempty"`
}

// One request shaped for the endpoint
type prepared struct {
	request *provider.Request
	route   provider.Route
	call    *provider.Call
	spec    provider.Spec
	where   string
	model   string
	body    map[string]any
	header  http.Header
	betas   []string
	// How the credential is sent: api_key, bearer, oauth for a Claude subscription, kimi for Kimi's coding endpoint, empty off the API
	style string
	// The request signs in with a Claude subscription, its tools under their wire names
	subscription bool
	// Names of the tools the request offers, which tool calls of a subscription reply are mapped back onto
	known map[string]bool
	// Tools sent under other names, by the name sent
	aliases map[string]string
	// Thinking blocks the request replays, by their fingerprints
	thinking    []string
	bedrockBase string
	tokens      int
	limits      transport.Limits
	refused     transport.Refused
	settings    config.View
	fast        bool
}

// Kind of the route as its provider has it, its settings resolved, else as its plugin registers it
func (c *client) kind(cfg *config.Config) provider.Spec {
	if instance, ok := c.r.providers.Instance(cfg, c.route.Provider); ok && instance.ID == c.route.Provider && instance.Spec.Name == c.route.Spec {
		return instance.Spec
	}
	spec, _ := c.r.providers.Spec(c.route.Spec)
	return spec
}

// Builds the body and headers of one request
func (c *client) prepare(ctx context.Context, request *provider.Request) (*prepared, error) {
	cfg := cmp.Or(request.Config, c.r.config)
	spec := c.kind(cfg)
	settings := cfg.View(providerconfig.Key).View(c.route.Provider)
	model := cmp.Or(strings.TrimSpace(request.Model), c.route.Model)
	where := platform(c.route)
	p := &prepared{request: request, route: c.route, spec: spec, where: where, settings: settings, known: map[string]bool{},
		bedrockBase: cfg.GetString(anthropicconfig.BedrockBaseURL)}
	p.call = &provider.Call{Context: ctx, Route: c.route, Model: model, Settings: settings, Config: cfg, SessionID: request.SessionID, Source: request.Source,
		Scope: request.SessionID, Ask: request.Reasoning.Ask(), UserTurn: request.UserTurn, Kind: c.r.providers.Spec}
	p.call.Thinking, _ = c.r.models.Thinking(ctx, cfg, c.route)
	p.style = authStyle(c.route, where)
	p.subscription = p.style == "oauth"

	for _, tool := range request.Tools {
		if function, ok := tool["function"].(map[string]any); ok {
			if name, _ := function["name"].(string); name != "" {
				p.known[name] = true
			}
		}
	}
	caching := c.r.registry.Caching(cfg, request, c.route, spec)
	messages, tools := caching.Plan(c.dicts(request), request.Tools)
	tools, p.aliases = transport.Reserved(tools, spec.ReservedTools)
	system, converted := convert(messages, c.route.BaseURL, model)
	refused := c.r.rejected.of(request.SessionID)
	if slices.Contains(signedRoutes, thinkingRoute(c.route.BaseURL, model)) {
		p.thinking, _ = strip(converted, refused, false)
	}
	anthropicTools := convertTools(tools)
	p.model = modelName(model, keepsDots(spec, c.route))

	limit := request.MaxTokens
	switch {
	case spec.OutputCap != nil:
		limit = spec.OutputCap(p.call, limit)
	case limit <= 0 && spec.DefaultMaxTokens > 0:
		limit = spec.DefaultMaxTokens
	}
	if limit <= 0 {
		limit = cfg.GetInt(anthropicconfig.MaxTokens)
	}
	if limit <= 0 {
		limit = outputLimit(p.model)
	}
	if window, ok := c.r.models.CachedContext(cfg, model, c.route.BaseURL); ok && window > 0 && limit > window {
		limit = max(window-1, 1)
	}
	var toWire func(string) string
	if p.subscription {
		toWire = wireNamer(anthropicTools)
		system = claudeCodeIdentity(system, anthropicTools, converted, toWire)
	}
	wire := make([]any, len(converted))
	for i, m := range converted {
		wire[i] = m
	}
	body := map[string]any{"model": p.model, "messages": wire, "max_tokens": limit}
	if !emptySystem(system) {
		body["system"] = system
	}
	if len(anthropicTools) > 0 {
		list := make([]any, len(anthropicTools))
		for i, t := range anthropicTools {
			if c.buffered(p) {
				t["eager_input_streaming"] = false
			}
			list[i] = t
		}
		body["tools"] = list
		body["tool_choice"] = map[string]any{"type": "auto"}
	}
	maps.Copy(body, provider.ClaudeThinking(p.call.Ask, p.model, limit))
	if temperature := spec.Temperature(p.model, request.Temperature); temperature != nil {
		if _, set := body["temperature"]; !set {
			body["temperature"] = *temperature
		}
	}
	if provider.ClaudeSamplingForbidden(p.model) {
		for _, field := range []string{"temperature", "top_p", "top_k"} {
			delete(body, field)
		}
	}
	if format := request.Format; format != nil {
		outputConfig(body)["format"] = map[string]any{"type": "json_schema", "schema": format.Schema}
	}
	headers := c.extra(p, body, request.Extra, toWire)
	if speed, _ := body["speed"].(string); speed == "fast" {
		p.fast = !thirdParty(c.route.BaseURL) && len(provider.FastFields(p.model, "", "", "priority")) > 0 && !c.r.slow(request.SessionID, p.model)
		if !p.fast {
			delete(body, "speed")
		}
	}
	if where == direct && provider.BaseHost(c.route.BaseURL) == "api.anthropic.com" && !p.subscription && cfg.GetBool(anthropicconfig.Fallbacks) {
		body["fallbacks"] = "default"
	}
	leaked := []string{}
	for _, field := range []string{"instructions", "input", "store", "parallel_tool_calls"} {
		if _, has := body[field]; has {
			delete(body, field)
			leaked = append(leaked, field)
		}
	}
	if len(leaked) > 0 {
		slog.Warn(fmt.Sprintf("Stripped Responses-only field(s) %v from an Anthropic Messages call; the request goes on without them.", leaked))
	}
	p.body = body
	p.tokens = transport.Tokens(body)
	p.betas = c.betas(p)
	p.header = c.headers(p, cfg, settings, headers)
	p.limits = transport.Limits{Config: cfg, Settings: settings, Model: model, Local: c.route.Local, Tokens: p.tokens, Deadline: request.Deadline}
	p.refused = transport.Refused{Route: c.route, Model: model, Spec: spec, Classify: request.Classify}
	return p, nil
}

func emptySystem(system any) bool {
	switch s := system.(type) {
	case nil:
		return true
	case string:
		return s == ""
	case []any:
		return len(s) == 0
	}
	return false
}

func outputConfig(body map[string]any) map[string]any {
	config, ok := body["output_config"].(map[string]any)
	if !ok {
		config = map[string]any{}
		body["output_config"] = config
	}
	return config
}

// Fields the request puts over the body: its own as they are, those it nests in extra_body, a response_format as the output format, and
// the headers it names; the headers
func (c *client) extra(p *prepared, body map[string]any, extra map[string]any, toWire func(string) string) map[string]string {
	headers := map[string]string{}
	var merge func(fields map[string]any)
	merge = func(fields map[string]any) {
		for key, value := range fields {
			switch key {
			case "extra_body":
				if nested, ok := value.(map[string]any); ok {
					merge(nested)
				}
			case "extra_headers":
				if table, ok := value.(map[string]any); ok {
					for name, v := range table {
						headers[name] = fmt.Sprint(v)
					}
				}
				if table, ok := value.(map[string]string); ok {
					maps.Copy(headers, table)
				}
			case "response_format":
				if format, ok := value.(map[string]any); ok {
					responseFormat(body, format)
				}
			case "tool_choice":
				body[key] = toolChoice(value, toWire)
				if body[key] == nil {
					delete(body, key)
					delete(body, "tools")
				}
			default:
				body[key] = value
			}
		}
	}
	merge(extra)
	return headers
}

// OpenAI response format as the output format
func responseFormat(body map[string]any, format map[string]any) {
	var schema any
	switch format["type"] {
	case "json_schema":
		spec, ok := format["json_schema"].(map[string]any)
		if !ok {
			return
		}
		if schema, ok = spec["schema"]; !ok {
			return
		}
	case "json_object":
		schema = map[string]any{"type": "object"}
	default:
		return
	}
	outputConfig(body)["format"] = map[string]any{"type": "json_schema", "schema": schema}
}

// Tool choice of an OpenAI shaped value: auto, any for required, a forced tool by its wire name; nil for none, which sends no tools
func toolChoice(value any, toWire func(string) string) any {
	choice := ""
	switch v := value.(type) {
	case string:
		choice = v
	case map[string]any:
		switch kind, _ := v["type"].(string); kind {
		case "auto", "required", "none", "any":
			choice = kind
		default:
			if function, ok := v["function"].(map[string]any); ok {
				choice, _ = function["name"].(string)
			} else {
				choice, _ = v["name"].(string)
			}
		}
	}
	switch choice {
	case "none":
		return nil
	case "", "auto":
		return map[string]any{"type": "auto"}
	case "required", "any":
		return map[string]any{"type": "any"}
	}
	if toWire != nil {
		choice = toWire(choice)
	}
	return map[string]any{"type": "tool", "name": choice}
}

// Betas of the request: the platform's, the subscription's, fast mode's and the server side fallback's
func (c *client) betas(p *prepared) []string {
	_, drop := c.r.drop1M.Load(c.key())
	list := betas(c.route, p.where, drop)
	if p.subscription {
		list = append(list, subscriptionBetas...)
	}
	if p.fast {
		list = append(list, fastBeta)
	}
	if _, ok := p.body["fallbacks"]; ok {
		list = append(list, fallbackBeta)
	}
	return list
}

// Headers of the request: icebath's user agent, the route's own, the kind's for the request, the extra headers the request and the
// provider name, the betas, and the credential as the endpoint takes it
func (c *client) headers(p *prepared, cfg *config.Config, settings config.View, extra map[string]string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", provider.UserAgent("IcebathAgent"))
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	if p.where == direct {
		h.Set("anthropic-version", apiVersion)
	}
	key, style := strings.TrimSpace(c.route.APIKey), p.style
	if style == "kimi" || provider.BaseHostMatches(c.route.BaseURL, "opencode.ai") {
		for name, value := range provider.Attribution() {
			h.Set(name, value)
		}
	}
	for name, value := range c.route.Headers {
		h.Set(name, value)
	}
	if p.spec.RequestHeaders != nil {
		for name, value := range p.spec.RequestHeaders(p.call) {
			h.Set(name, value)
		}
	}
	transport.Extra(h, cfg, settings, p.request.SessionID, false)
	for name, value := range extra {
		h.Set(name, value)
	}
	if len(p.betas) > 0 && p.where != onBedrock {
		h.Set("anthropic-beta", strings.Join(p.betas, ","))
	}
	if style == "oauth" {
		h.Set("User-Agent", c.r.claude.userAgent())
		h.Set("x-app", "cli")
	}
	switch {
	case key == "" || c.route.Sign != nil:
	case p.where != direct, style == "oauth", style == "bearer":
		h.Del("X-Api-Key")
		h.Set("Authorization", "Bearer "+key)
	default:
		h.Del("Authorization")
		h.Set("X-Api-Key", key)
	}
	return h
}

// How the route's credential is sent: off the API by the platform; an OAuth token as a bearer, as a Claude subscription's on Anthropic's own
// host; a key as x-api-key, as a bearer to the endpoints that want one, and as a subscription's when it is a Claude sign-in token
func authStyle(route provider.Route, where string) string {
	key := strings.TrimSpace(route.APIKey)
	switch {
	case where != direct:
		return ""
	case route.Auth == provider.AuthOAuth && nativeHost(route.BaseURL) && oauthToken(key):
		return "oauth"
	case route.Auth == provider.AuthOAuth:
		return "bearer"
	case kimiCoding(route.BaseURL):
		return "kimi"
	case bearer(normalized(route.BaseURL)):
		return "bearer"
	case thirdParty(route.BaseURL):
		return "api_key"
	case oauthToken(key):
		return "oauth"
	}
	return "api_key"
}

// OpenAI shaped messages of the request, assistant turns with what their replies carried
func (c *client) dicts(request *provider.Request) []map[string]any {
	out := make([]map[string]any, 0, len(request.Messages))
	for _, m := range request.Messages {
		switch m.GetRole() {
		case sessionsv1.Role_ROLE_SYSTEM:
			out = append(out, map[string]any{"role": "system", "content": m.GetText()})
		case sessionsv1.Role_ROLE_USER:
			out = append(out, map[string]any{"role": "user", "content": content(m.GetText(), m.GetMedia())})
		case sessionsv1.Role_ROLE_TOOL:
			out = append(out, map[string]any{"role": "tool", "tool_call_id": m.GetToolCallId(), "content": content(m.GetText(), m.GetMedia())})
		case sessionsv1.Role_ROLE_ASSISTANT:
			out = append(out, assistantDict(m))
		}
	}
	return out
}

// Text of a message, or its text and its pictures as parts, a note in place of media the API can't read
func content(text string, media []*sessionsv1.Media) any {
	if len(media) == 0 {
		return text
	}
	parts := []any{map[string]any{"type": "text", "text": text}}
	for _, item := range media {
		mime := item.GetMimeType()
		if !strings.HasPrefix(mime, "image/") {
			noun, _, _ := strings.Cut(mime, "/")
			parts = append(parts, map[string]any{"type": "text", "text": "[" + noun + " omitted: " + mime + " is not a supported " + noun + " format]"})
			continue
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," +
			base64.StdEncoding.EncodeToString(item.GetData())}})
	}
	return parts
}

// Whether a Converse turn was made by a model other than Claude, whose thinking Claude can't take; an application inference profile no one
// resolved counts as unknown
func foreign(issuer string) bool {
	lower := strings.ToLower(issuer)
	return !strings.Contains(lower, "claude") && !strings.Contains(lower, ":application-inference-profile/")
}

// Replayed assistant turn: its text, calls and reasoning, and the blocks its reply carried, those of a Claude turn made through Bedrock
// Converse when it was made there
func assistantDict(m *sessionsv1.Message) map[string]any {
	out := map[string]any{"role": "assistant", "content": m.GetText()}
	var calls []any
	for _, call := range m.GetToolCalls() {
		calls = append(calls, map[string]any{"id": call.GetId(), "type": "function", "function": map[string]any{"name": call.GetName(),
			"arguments": call.GetArguments()}})
	}
	if len(calls) > 0 {
		out["tool_calls"] = calls
	}
	if m.GetReasoning() != "" {
		out["reasoning_content"] = m.GetReasoning()
	}
	kept := transport.Carried[carried](m, carrier)
	if len(kept.Details) == 0 && len(kept.Blocks) == 0 {
		converse := transport.Carried[carried](m, converseCarrier)
		for _, d := range converse.Details {
			if detail, ok := d.(map[string]any); ok && detail["type"] == issuerRecord && foreign(transport.Text(detail["model"])) {
				out["foreign_issuer"] = true
			}
		}
		kept.Details = converse.Details
	}
	if len(kept.Details) > 0 {
		out["reasoning_details"] = transport.DeepCopy(kept.Details)
	}
	if len(kept.Blocks) > 0 {
		out["anthropic_content_blocks"] = transport.DeepCopy(kept.Blocks)
	}
	return out
}
