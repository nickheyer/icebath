package transport

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	codexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/transports/codexresponses/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Name the web_search tool goes by on xAI when the person's own search backend runs it, since xAI keeps web_search for its own
const xaiWebSearch = "icebath_web_search"

// One request shaped for the Responses API
type prepared struct {
	request *provider.Request
	route   provider.Route
	call    *provider.Call
	spec    provider.Spec
	// Kind claiming the address, the route's own when none does
	hosted   provider.Spec
	settings config.View
	cfg      *config.Config
	// ChatGPT's Codex backend, xAI's Responses API
	codex, xai bool
	// Model the body names, and the endpoint its blobs are sealed to
	model, issuer string
	body          map[string]any
	header        http.Header
	url           string
	aliases       map[string]string
	tokens        int
	limits        transport.Limits
	refused       transport.Refused
	// The body asks for every turn's reasoning, which a model may refuse
	allTurns bool
	// The body is compressed, as the official Codex backend takes it
	compress bool
}

// Kind of the route as its provider has it, its settings resolved, else as its plugin registers it
func (c *client) kind(cfg *config.Config) provider.Spec {
	if instance, ok := c.r.providers.Instance(cfg, c.route.Provider); ok && instance.ID == c.route.Provider && instance.Spec.Name == c.route.Spec {
		return instance.Spec
	}
	spec, _ := c.r.providers.Spec(c.route.Spec)
	return spec
}

func azure(base string) bool {
	return provider.BaseHostMatches(base, "openai.azure.com") || provider.BaseHostMatches(base, "services.ai.azure.com")
}

func (c *client) prepare(ctx context.Context, request *provider.Request) (*prepared, error) {
	cfg := cmp.Or(request.Config, c.r.config)
	spec := c.kind(cfg)
	hosted := spec
	if claimed := c.r.models.Kind(c.route.BaseURL); claimed != "" && claimed != spec.Name {
		hosted, _ = c.r.providers.Spec(claimed)
	}
	if provider.ModeForURL(c.route.BaseURL) == provider.ModeChatCompletions {
		return nil, fmt.Errorf("%s requests require Chat Completions; refusing to call /responses.", cmp.Or(spec.Label(), c.route.Provider))
	}
	settings := cfg.View(providerconfig.Key).View(c.route.Provider)
	base := strings.ToLower(c.route.BaseURL)
	p := &prepared{request: request, route: c.route, spec: spec, hosted: hosted, settings: settings, cfg: cfg}
	p.codex = hosted.ResponsesIssuer == "codex_backend" || spec.ResponsesIssuer == "codex_backend" ||
		provider.BaseHostMatches(base, "chatgpt.com") && strings.Contains(base, "/backend-api/codex")
	p.xai = hosted.ResponsesIssuer == "xai_responses" || spec.ResponsesIssuer == "xai_responses" || provider.BaseHost(base) == "api.x.ai"
	p.issuer = issuerOf(p)
	model := cmp.Or(request.Model, c.route.Model)
	wire := model
	if override, _ := request.Extra["model"].(string); override != "" {
		wire = override
	}
	p.model = identity(wire)
	p.call = &provider.Call{Context: ctx, Route: c.route, Model: model, Settings: settings, Config: cfg, SessionID: request.SessionID, Source: request.Source,
		Scope: transport.Scope(request.SessionID), Ask: request.Reasoning.Ask(), UserTurn: request.UserTurn, Kind: c.r.providers.Spec}
	p.call.Thinking, _ = c.r.models.Thinking(ctx, cfg, c.route)

	instructions, payload := "", request.Messages
	if len(payload) > 0 && payload[0].GetRole() == sessionsv1.Role_ROLE_SYSTEM {
		instructions, payload = strings.TrimSpace(payload[0].GetText()), payload[1:]
	}
	instructions = cmp.Or(instructions, provider.Identity)
	sanitize := func(s string) string { return s }
	if p.codex {
		sanitize = neutralize
	}
	_, compaction := request.Extra["context_management"]
	compaction = compaction && !hosted.ResponsesNoNativeCompaction
	encrypted := !c.r.off(request.SessionID, p.model) && !(provider.BaseHostMatches(base, "services.ai.azure.com") && afterTools(payload))
	rules := replayRules{issuer: p.issuer, model: p.model, encrypted: encrypted, ids: !hosted.ResponsesDropReplayItemIDs, compaction: compaction,
		typed: p.issuer == "codex_backend", sanitize: sanitize, newestOnly: encrypted && azure(base)}
	items := input(payload, rules)
	if compaction {
		items = pruneCheckpointed(items)
	}
	tools, aliases := c.tools(p, request.Tools)
	p.aliases = aliases
	body := map[string]any{"model": p.model, "instructions": sanitize(instructions), "input": items, "store": false}
	if len(tools) > 0 {
		body["tools"], body["tool_choice"], body["parallel_tool_calls"] = tools, "auto", true
	}
	scope := transport.Scope(request.SessionID)
	key := cmp.Or(transport.CacheKey(instructions, tools, scope), scope)
	if !hosted.ResponsesOmitPromptCacheKey && !p.xai && key != "" {
		body["prompt_cache_key"] = key
	}
	if retention := cacheRetention(model, c.route.BaseURL); retention != "" {
		body["prompt_cache_retention"] = retention
	}
	effort, enabled := c.reasoning(ctx, p, model)
	p.allTurns = enabled && !p.codex && provider.BaseHost(base) == "api.openai.com" && allTurnsFamily(p.model) != "" && !c.r.rejectsAllTurns(p.model)
	maps.Copy(body, c.reasoningFields(p, effort, enabled, encrypted))
	if verbosity := cfg.GetString(codexconfig.TextVerbosity); verbosity != "" && !p.xai {
		body["text"] = map[string]any{"verbosity": verbosity}
	}
	if temperature := spec.Temperature(model, request.Temperature); temperature != nil {
		body["temperature"] = *temperature
	}
	if format := request.Format; format != nil {
		text, _ := body["text"].(map[string]any)
		if text == nil {
			text = map[string]any{}
		}
		text["format"] = map[string]any{"type": "json_schema", "name": format.Name, "schema": format.Schema, "strict": format.Strict}
		body["text"] = text
	}
	headers := map[string]string{}
	for k, v := range request.Extra {
		switch k {
		case "extra_body":
			if nested, ok := v.(map[string]any); ok {
				maps.Copy(body, nested)
			}
		case "extra_headers":
			if table, ok := v.(map[string]any); ok {
				for name, value := range table {
					headers[name] = fmt.Sprint(value)
				}
			}
		case "context_management":
			if compaction {
				body[k] = v
			}
		default:
			body[k] = v
		}
	}
	body["model"] = p.model
	if _, ok := body["prompt_cache_options"]; ok {
		delete(body, "prompt_cache_options")
		slog.Warn("Dropped prompt_cache_options: not a Responses field (use extra_body for wire-only fields).")
	}
	astra(body, model, c.route.BaseURL)
	if key, ok := body["prompt_cache_key"].(string); ok {
		if bounded := transport.BoundedKey(key); bounded != "" {
			body["prompt_cache_key"] = bounded
		} else {
			delete(body, "prompt_cache_key")
		}
	}
	if p.xai && !(provider.Grok46(model) && body["service_tier"] == "priority") {
		delete(body, "service_tier")
	}
	switch {
	case p.codex:
		if request.SessionID != "" {
			headers["session_id"] = request.SessionID
		}
		if key, _ := body["prompt_cache_key"].(string); key != "" {
			headers["x-client-request-id"] = key
		} else if bounded := transport.BoundedKey(scope); bounded != "" {
			headers["x-client-request-id"] = bounded
		}
		if _, ok := body["prompt_cache_retention"]; ok {
			delete(body, "prompt_cache_retention")
			slog.Warn(fmt.Sprintf("Dropped unsupported prompt_cache_retention at consumer Codex wire boundary (model=%s).", p.model))
		}
	case request.MaxTokens > 0:
		body["max_output_tokens"] = request.MaxTokens
	}
	if p.xai && request.SessionID != "" {
		headers["x-grok-conv-id"] = scope
		if _, ok := body["prompt_cache_key"]; !ok {
			body["prompt_cache_key"] = transport.BoundedKey(key)
		}
	}
	if spec.Finish != nil {
		spec.Finish(p.call, body)
	}
	p.body = body
	p.tokens = transport.Tokens(body)
	address, err := transport.Endpoint(c.route.BaseURL, "/responses")
	if err != nil {
		return nil, err
	}
	p.url = address
	p.compress = provider.OfficialCodex(c.route.BaseURL)
	p.header = c.headers(p, headers)
	p.limits = transport.Limits{Config: cfg, Settings: settings, Model: model, Local: c.route.Local, Tokens: p.tokens, Deadline: request.Deadline}
	p.refused = transport.Refused{Route: c.route, Model: model, Spec: spec, Classify: request.Classify}
	return p, nil
}

// Headers of the request: icebath's user agent, the route's own, those the kind and the kind claiming its address set for the request, the
// request's own, those every request to the provider carries and the credential; on the official Codex backend the kinds' again, its
// identity kept over anything that replaced it
func (c *client) headers(p *prepared, extra map[string]string) http.Header {
	h := http.Header{}
	agent := "IcebathAgent"
	if p.xai {
		agent = "Icebath-Agent"
	}
	h.Set("User-Agent", provider.UserAgent(agent))
	kinds := []provider.Spec{p.spec}
	if p.hosted.Name != p.spec.Name {
		kinds = append(kinds, p.hosted)
	}
	own := func() {
		for _, kind := range kinds {
			if kind.RequestHeaders != nil {
				for name, value := range kind.RequestHeaders(p.call) {
					h.Set(name, value)
				}
			}
		}
	}
	for name, value := range c.route.Headers {
		h.Set(name, value)
	}
	own()
	for name, value := range extra {
		h.Set(name, value)
	}
	transport.Extra(h, p.cfg, p.settings, p.request.SessionID, true)
	if key := strings.TrimSpace(c.route.APIKey); key != "" && c.route.Sign == nil {
		h.Set("Authorization", "Bearer "+key)
		if azure(c.route.BaseURL) || provider.BaseHostMatches(c.route.BaseURL, "cognitiveservices.azure.com") {
			h.Set("api-key", key)
		}
	}
	if p.compress {
		own()
	}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	return h
}

// Tools of the request as the Responses API takes them: web_search swapped for the provider's own server side search when the person's
// search backend is the provider's, or sent under another name on xAI where its own search keeps the name; the names the kind keeps for its
// own tools sent under other names
func (c *client) tools(p *prepared, defs []map[string]any) ([]map[string]any, map[string]string) {
	var tools []map[string]any
	for _, def := range defs {
		function, _ := def["function"].(map[string]any)
		name, _ := function["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		strict, _ := function["strict"].(bool)
		parameters, has := function["parameters"]
		if !has {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		description, _ := function["description"].(string)
		tools = append(tools, map[string]any{"type": "function", "name": name, "description": description, "strict": strict, "parameters": parameters})
	}
	aliases := map[string]string{}
	at := slices.IndexFunc(tools, func(tool map[string]any) bool { return tool["name"] == "web_search" })
	backend := strings.ToLower(strings.TrimSpace(p.request.WebSearch))
	switch {
	case at < 0:
	case p.xai && (backend == "" || backend == "xai"), p.codex && backend == "openai-native":
		tools = append(slices.Delete(slices.Clone(tools), at, at+1), map[string]any{"type": "web_search"})
	case p.xai:
		tools = slices.Clone(tools)
		tools[at] = transport.Renamed(tools[at], xaiWebSearch)
		aliases[xaiWebSearch] = "web_search"
	}
	renamed, reserved := transport.Reserved(tools, slices.Concat(p.spec.ReservedTools, p.hosted.ReservedTools))
	maps.Copy(aliases, reserved)
	return renamed, aliases
}

// Effort a request reasons at and whether it reasons: medium unless asked otherwise, clamped onto what the route takes, which on xAI is the
// kind's list, else the kind's own declared list, else OpenAI's for the model's generation; off when asked none and the route takes none
func (c *client) reasoning(ctx context.Context, p *prepared, model string) (string, bool) {
	ask := p.call.Ask
	effort := cmp.Or(ask.Effort, "medium")
	var supported []string
	declared := false
	official := p.codex || provider.BaseHost(p.route.BaseURL) == "api.openai.com"
	for _, spec := range []provider.Spec{p.hosted, p.spec} {
		if spec.Efforts != nil && (p.xai || !official) && !declared {
			supported, declared = spec.Efforts(ctx, model)
		}
	}
	switch {
	case p.xai:
		if len(supported) == 0 {
			return "", !ask.Off
		}
	case declared:
	case !p.codex && provider.BaseHost(p.route.BaseURL) == "api.openai.com" && rejectsReasoning(model):
		supported = nil
	default:
		supported = provider.ResponsesEfforts(model, p.codex || provider.BaseHost(p.route.BaseURL) == "api.openai.com")
	}
	if len(supported) == 0 && !p.xai {
		return "", false
	}
	if ask.Off {
		if slices.Contains(supported, "none") {
			return "none", false
		}
		if _, warned := c.r.warned.LoadOrStore(model, true); !warned {
			slog.Warn(fmt.Sprintf("reasoning_effort: none cannot be sent for %s — its route accepts only %s, so the model's default effort stays on "+
				"(an omitted reasoning field does not disable it).", model, strings.Join(supported, ", ")))
		}
		return "", false
	}
	return provider.ClampEffort(effort, supported, nil), true
}

// Whether an OpenAI model of the chat era refuses any reasoning field: gpt-3.5, gpt-4 with no digit after it, chatgpt-, fine tunes of them too
func rejectsReasoning(model string) bool {
	name := strings.TrimPrefix(provider.BareModel(model), "ft:")
	if rest, ok := strings.CutPrefix(name, "gpt-4"); ok {
		return rest == "" || rest[0] < '0' || rest[0] > '9'
	}
	return strings.HasPrefix(name, "gpt-3.5") || strings.HasPrefix(name, "chatgpt-")
}

// Reasoning and include fields of the body: the effort with a summary and the encrypted reasoning back, every turn's reasoning on the models
// of api.openai.com that keep it; on xAI the encrypted reasoning and the effort where its model takes one; the kind's own reasoning field
// where it gives one; none, or effort none, when off
func (c *client) reasoningFields(p *prepared, effort string, enabled, encrypted bool) map[string]any {
	include := []any{}
	if encrypted {
		include = []any{"reasoning.encrypted_content"}
	}
	fields := map[string]any{}
	profile := p.hosted.ResponsesReasoningFromProfile || p.spec.ResponsesReasoningFromProfile
	switch {
	case enabled && p.xai:
		fields["include"] = include
		if effort != "" {
			fields["reasoning"] = map[string]any{"effort": effort}
		}
	case enabled && profile:
		for _, spec := range []provider.Spec{p.spec, p.hosted} {
			if spec.ReasoningPayload != nil {
				if payload := spec.ReasoningPayload(p.call); payload != nil {
					reasoning := maps.Clone(payload)
					reasoning["summary"] = "auto"
					fields["reasoning"] = reasoning
				}
				break
			}
		}
	case enabled:
		reasoning := map[string]any{"effort": effort, "summary": "auto"}
		if p.allTurns {
			reasoning["context"] = "all_turns"
		}
		fields["reasoning"] = reasoning
		fields["include"] = include
	case !profile && !p.xai:
		fields["include"] = []any{}
		if effort == "none" {
			fields["reasoning"] = map[string]any{"effort": "none"}
		}
	}
	return fields
}

// Models whose prompts Bedrock Mantle keeps cached for a day when asked
var extendedCache = regexp.MustCompile(`(?:^|[./:])(?:gpt-5\.5-pro|gpt-5\.5|gpt-5\.4|gpt-5\.2|gpt-5\.1-codex-max|gpt-5\.1-codex-mini|gpt-5\.1-chat-latest|` +
	`gpt-5\.1-codex|gpt-5\.1|gpt-5-codex|gpt-5|gpt-4\.1)(?:-\d{4}-\d{2}-\d{2})?$`)

// Prompt cache retention of a request: a day on Meta's API and for the models Bedrock Mantle keeps that long, the endpoint's own elsewhere
func cacheRetention(model, base string) string {
	host := provider.BaseHost(base)
	if host == "api.meta.ai" {
		return "24h"
	}
	parts := strings.Split(host, ".")
	if len(parts) == 4 && parts[0] == "bedrock-mantle" && parts[1] != "" && parts[2] == "api" && parts[3] == "aws" &&
		extendedCache.MatchString(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), "_", "-")) {
		return "24h"
	}
	return ""
}

// The official API's terms for GPT-6 Astra: its effort clamped onto the efforts it takes, low when none, and no sampling, logprobs or cache
// retention
func astra(body map[string]any, model, base string) {
	if !provider.Astra(model) || provider.BaseHost(base) != "api.openai.com" {
		return
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if requested, _ := reasoning["effort"].(string); strings.TrimSpace(requested) != "" {
			reasoning["effort"] = provider.ClampEffort(strings.ToLower(strings.TrimSpace(requested)), provider.CodexEfforts(model), nil)
		} else {
			reasoning["effort"] = "low"
		}
	}
	for _, key := range []string{"temperature", "top_p", "top_logprobs", "logprobs", "prompt_cache_retention"} {
		delete(body, key)
	}
	if include, ok := body["include"].([]any); ok {
		body["include"] = slices.DeleteFunc(slices.Clone(include), func(item any) bool {
			s, _ := item.(string)
			return strings.Contains(strings.ToLower(s), "logprob")
		})
	}
}
