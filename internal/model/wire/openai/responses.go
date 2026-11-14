package openai

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Alias a client web_search takes on xAI, where the name collides with the native search
const xaiWebSearchAlias = reservedAliasPrefix + "web_search"

// Models whose prompt cache a Bedrock Mantle endpoint keeps for a day when asked
var extendedCacheModel = regexp.MustCompile(`(?:^|[./:])(?:gpt-5\.5-pro|gpt-5\.5|gpt-5\.4|gpt-5\.2|gpt-5\.1-codex-max|` +
	`gpt-5\.1-codex-mini|gpt-5\.1-chat-latest|gpt-5\.1-codex|gpt-5\.1|gpt-5-codex|gpt-5|gpt-4\.1)(?:-\d{4}-\d{2}-\d{2})?$`)

// Harmony control tokens the ChatGPT Codex backend rejects when they appear literally
var harmonyToken = regexp.MustCompile(`<\|(start|end|channel|message|constrain|return|call)\|>`)

// OpenAI chat-era model families that reject any reasoning member on api.openai.com
var nonReasoningModel = regexp.MustCompile(`^(?:ft:)?(?:gpt-3\.5|gpt-4(?:[^0-9]|$)|chatgpt-)`)

// Responses protocol, holding only the once-per-process warning marks of its builds
type ResponsesProtocol struct {
	crossIssuerWarned  atomic.Bool
	cacheOptionsWarned atomic.Bool
	mu                 sync.Mutex
	disableWarned      map[string]bool
}

// Bare lowercase model id with any vendor prefix removed
func bareModel(model string) string {
	name := strings.ToLower(strip(model))
	return name[strings.LastIndex(name, "/")+1:]
}

// Reports whether a model is in the Grok 4.6 family
func isGrok46(model string) bool {
	name := strings.ToLower(strip(model))
	name = strings.ReplaceAll(name, "_", "-")
	name = name[strings.LastIndex(name, "/")+1:]
	return name == "grok-4.6" || strings.HasPrefix(name, "grok-4.6-")
}

// Reports whether a Grok model accepts a reasoning effort
func grokTakesEffort(model string) bool {
	name := bareModel(model)
	for _, prefix := range [...]string{"grok-3-mini", "grok-4.20-multi-agent", "grok-4.3", "grok-4.5", "grok-4.6"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// Host of an endpoint URL, empty when it has none
func hostOf(raw string) string {
	_, rest, found := strings.Cut(raw, "://")
	if !found {
		return ""
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	rest = rest[strings.LastIndex(rest, "@")+1:]
	if strings.HasPrefix(rest, "[") {
		if end := strings.Index(rest, "]"); end > 0 {
			return rest[1:end]
		}
		return ""
	}
	host, _, _ := strings.Cut(rest, ":")
	return host
}

// Endpoint URL with only its proven-equivalent parts folded, scheme and host case, default port, trailing slash
func normalizeRouteURL(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] <= 0x20 {
			return raw
		}
	}
	scheme, rest, found := strings.Cut(raw, "://")
	if !found || scheme == "" {
		return raw
	}
	for i, r := range scheme {
		if !(unicode.IsLetter(r) || (i > 0 && (unicode.IsDigit(r) || strings.ContainsRune("+-.", r)))) || r >= utf8.RuneSelf {
			return raw
		}
	}
	scheme = strings.ToLower(scheme)
	netloc, tail := rest, ""
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		netloc, tail = rest[:end], rest[end:]
	}
	tail, _, _ = strings.Cut(tail, "#")
	path, query, hadQuery := strings.Cut(tail, "?")
	userinfo, hostport := "", netloc
	if at := strings.LastIndex(netloc, "@"); at >= 0 {
		userinfo, hostport = netloc[:at], netloc[at+1:]
	}
	host, port := hostport, ""
	bracketed := strings.HasPrefix(hostport, "[")
	if bracketed {
		end := strings.Index(hostport, "]")
		if end < 0 {
			return raw
		}
		host, port = hostport[1:end], strings.TrimPrefix(hostport[end+1:], ":")
	} else if colon := strings.Index(hostport, ":"); colon >= 0 {
		host, port = hostport[:colon], hostport[colon+1:]
	}
	if host == "" {
		return raw
	}
	if address, zone, zoned := strings.Cut(host, "%"); zoned {
		host = strings.ToLower(address) + "%" + zone
	} else {
		host = strings.ToLower(host)
	}
	if bracketed || strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return raw
		}
		if !(scheme == "http" && n == 80) && !(scheme == "https" && n == 443) {
			host += ":" + strconv.Itoa(n)
		}
	}
	if userinfo != "" || strings.Contains(netloc, "@") {
		host = userinfo + "@" + host
	}
	if strings.HasSuffix(path, "/") && !hadQuery {
		path = path[:len(path)-1]
	}
	out := scheme + "://" + host + path
	if hadQuery {
		out += "?" + query
	}
	return out
}

// Stable name of the endpoint that seals encrypted reasoning, from its backend, a declared kind or its URL
func IssuerKind(backend wire.ResponsesBackend, declared, baseURL string) string {
	switch {
	case backend == wire.ResponsesXAI:
		return "xai_responses"
	case declared != "":
		return declared
	case backend == wire.ResponsesCodex:
		return "codex_backend"
	case baseURL == "":
		return "other"
	}
	return "other:" + normalizeRouteURL(strip(baseURL))
}

// Reasoning levels a route accepts when no profile declares them, none for a model that takes no reasoning member
func routeEfforts(model string, backend wire.ResponsesBackend) []string {
	codex, official := backend == wire.ResponsesCodex, backend == wire.ResponsesOpenAI
	if official && nonReasoningModel.MatchString(bareModel(model)) {
		return []string{}
	}
	if wire.IsAstraModel(model) && !codex && !official {
		return wire.EffortsCodexLegacy.Efforts().Levels
	}
	return wire.CodexEfforts(model).Efforts().Levels
}

// Effort and enabled state of a request's reasoning, the effort clamped onto the route's levels
func (p *ResponsesProtocol) resolveReasoning(req *wire.Request) (effort string, enabled bool) {
	return p.ResolveEffort(req.Model, req.Reasoning, req.Responses.Backend, req.Responses.Efforts)
}

func (p *ResponsesProtocol) ResolveEffort(model string, r *wire.Reasoning, backend wire.ResponsesBackend, declared []string) (effort string, enabled bool) {
	effort, enabled = "medium", true
	if r != nil {
		if !r.Enabled {
			enabled = false
		} else if r.Effort != "" {
			effort = r.Effort
		}
	}
	var supported []string
	switch {
	case backend == wire.ResponsesXAI && isGrok46(model):
		supported = wire.EffortsXAIGrok46.Efforts().Levels
	case backend == wire.ResponsesXAI:
		supported = wire.EffortsXAILegacy.Efforts().Levels
	default:
		if backend != wire.ResponsesCodex && backend != wire.ResponsesOpenAI {
			supported = declared
		}
		if supported == nil {
			supported = routeEfforts(model, backend)
		}
		if len(supported) == 0 {
			return "", false
		}
	}
	if enabled {
		return wire.ClampEffort(effort, supported, nil), true
	}
	for _, level := range supported {
		if strings.ToLower(strip(level)) == "none" {
			return "none", false
		}
	}
	p.mu.Lock()
	warned := p.disableWarned[model]
	if !warned {
		if p.disableWarned == nil {
			p.disableWarned = map[string]bool{}
		}
		p.disableWarned[model] = true
	}
	p.mu.Unlock()
	if !warned {
		slog.Warn("a reasoning disable has no wire form on this route, the model's default effort stays on",
			"model", model, "accepts", strings.Join(supported, ", "))
	}
	return "", false
}

// Responses function tools for the request's tools, with provider-reserved names aliased and the aliases issued
func responsesTools(req *wire.Request) ([]json.RawMessage, map[string]string) {
	o := &req.Responses
	xai, codex := o.Backend == wire.ResponsesXAI, o.Backend == wire.ResponsesCodex
	aliases := map[string]string{}
	var names []string
	var tools []*wire.Dict
	for i := range req.Tools {
		t := &req.Tools[i]
		switch {
		case t.Builtin != "":
			names, tools = append(names, ""), append(tools, (&wire.Dict{}).SetString("type", t.Builtin))
		case strip(t.Name) != "":
			params := t.Parameters
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			names = append(names, t.Name)
			tools = append(tools, (&wire.Dict{}).SetString("type", "function").SetString("name", t.Name).
				SetString("description", t.Description).SetBool("strict", false).Set("parameters", params))
		}
	}
	client := slices.Index(names, "web_search")
	if client >= 0 && ((xai && o.NativeWebSearch) || (codex && o.NativeWebSearch)) {
		for client >= 0 {
			names, tools = slices.Delete(names, client, client+1), slices.Delete(tools, client, client+1)
			client = slices.Index(names, "web_search")
		}
		names, tools = append(names, ""), append(tools, (&wire.Dict{}).SetString("type", "web_search"))
	} else if client >= 0 && xai {
		for i, name := range names {
			if name == "web_search" {
				names[i] = xaiWebSearchAlias
			}
		}
		aliases[xaiWebSearchAlias] = "web_search"
	}
	if o.Backend == wire.ResponsesOpenCode {
		aliasReserved(names, []string{"web_search", "search_files"}, aliases)
	}
	if o.Backend == wire.ResponsesPerplexity {
		aliasReserved(names, []string{"web_search", "search_files", "fetch_url", "people_search", "finance_search"}, aliases)
	}
	if xai || codex || o.Backend == wire.ResponsesOpenAI {
		aliasReserved(names, []string{"tool_search"}, aliases)
	}
	out := make([]json.RawMessage, len(tools))
	for i, t := range tools {
		if names[i] != "" {
			t.SetString("name", names[i])
		}
		out[i] = t.JSON()
	}
	return out, aliases
}

// reasoning and include members for the endpoint family
func reasoningFields(body *wire.Dict, req *wire.Request, effort string, enabled, replay, allTurns bool) {
	o := &req.Responses
	xai := o.Backend == wire.ResponsesXAI
	include := json.RawMessage("[]")
	if replay {
		include = json.RawMessage(`["reasoning.encrypted_content"]`)
	}
	switch {
	case enabled && xai:
		body.Set("include", include)
		if grokTakesEffort(req.Model) {
			body.Set("reasoning", (&wire.Dict{}).SetString("effort", effort).JSON())
		}
	case enabled && o.ProfileReasoning:
		if req.Reasoning != nil && len(req.Reasoning.Payload) > 0 {
			if payload, err := wire.ParseValue(req.Reasoning.Payload); err == nil && payload.IsObject() {
				body.Set("reasoning", wire.DictOf(payload).SetString("summary", "auto").JSON())
			}
		}
	case enabled:
		reasoning := (&wire.Dict{}).SetString("effort", effort).SetString("summary", "auto")
		if allTurns {
			reasoning.SetString("context", "all_turns")
		}
		body.Set("reasoning", reasoning.JSON()).Set("include", include)
	case !o.ProfileReasoning && !xai:
		body.Set("include", json.RawMessage("[]"))
		if effort == "none" {
			body.Set("reasoning", json.RawMessage(`{"effort":"none"}`))
		}
	}
}

// Applies the official GPT-6 Astra contract after overrides, efforts low to max only and no sampling or logprob members
func sanitizeAstra(body *wire.Dict) {
	if reasoning := body.Get("reasoning"); reasoning.IsObject() {
		requested := strings.ToLower(strip(displayOrEmpty(reasoning.Get("effort"))))
		level := "low"
		if requested != "" {
			level = wire.EffortsCodexAstra.Efforts().Clamp(requested)
		}
		body.Set("reasoning", wire.DictOf(reasoning).SetString("effort", level).JSON())
	}
	for _, key := range [...]string{"temperature", "top_p", "top_logprobs", "logprobs", "prompt_cache_retention"} {
		body.Delete(key)
	}
	if include := body.Get("include"); include.IsArray() {
		var kept []json.RawMessage
		for _, item := range include.Elems() {
			if !strings.Contains(strings.ToLower(item.Display()), "logprob") {
				kept = append(kept, item.Raw())
			}
		}
		body.Set("include", wire.Array(kept...))
	}
}

// tool_choice value in the Responses form
func responsesToolChoice(c wire.ToolChoice) json.RawMessage {
	switch {
	case c.Name != "":
		return (&wire.Dict{}).SetString("type", "function").SetString("name", c.Name).JSON()
	case c.Mode != "":
		return wire.Quote(c.Mode)
	}
	return wire.Quote("auto")
}

// text member for verbosity and structured output, nil when the request asks for neither
func responsesText(req *wire.Request, xai bool) json.RawMessage {
	text := &wire.Dict{}
	if req.Verbosity != "" && !xai {
		text.SetString("verbosity", req.Verbosity)
	}
	if f := req.ResponseFormat; f != nil {
		format := (&wire.Dict{}).SetString("type", f.Type)
		if f.Type == "json_schema" {
			format.SetString("name", f.Name)
			if len(f.Schema) > 0 {
				format.Set("schema", f.Schema)
			}
			if f.Strict != nil {
				format.SetBool("strict", *f.Strict)
			}
		}
		text.Set("format", format.JSON())
	}
	if text.Len() == 0 {
		return nil
	}
	return text.JSON()
}

// Builds a Responses call, converting history to input items and validating the body before it is sent
func (p *ResponsesProtocol) Build(req *wire.Request) (*wire.Call, error) {
	o := &req.Responses
	codex, xai := o.Backend == wire.ResponsesCodex, o.Backend == wire.ResponsesXAI
	instructions, messages := req.Instructions, req.Messages
	if instructions == "" && len(messages) > 0 && messages[0].Role == wire.RoleSystem {
		instructions, messages = strip(messages[0].FlatText("\n")), messages[1:]
	}
	if instructions == "" {
		instructions = o.Identity
	}
	model := req.Model
	if override, err := wire.ParseValue(req.TopLevel["model"]); err == nil && override.Text() != "" {
		model = override.Text()
	}
	replay := o.ReplayEncrypted && !(o.Backend == wire.ResponsesAzureFoundry && postToolReplay(messages))
	if replay && o.NewestOnly {
		messages = newestReasoningOnly(messages)
	}
	management, _ := wire.ParseValue(req.ContextManagement)
	native := management.IsArray() && len(management.Elems()) > 0
	if native && o.Prune == nil {
		return nil, &RequestError{"Responses native compaction needs an input pruner."}
	}
	effort, enabled := p.resolveReasoning(req)
	tools, aliases := responsesTools(req)
	issuerModel := o.IssuerModel
	if issuerModel == "" {
		issuerModel = strip(model)
	}
	conv := &inputConv{p: p, o: o, issuer: o.IssuerKind, model: issuerModel, replay: replay, native: native,
		seenItems: map[string]bool{}, seenCalls: map[string]int{}, queue: map[string][]string{}}
	input, err := conv.convert(messages)
	if err != nil {
		return nil, err
	}
	body := (&wire.Dict{}).SetString("model", model).SetString("instructions", instructions).
		Set("input", wire.Array(input...)).SetBool("store", req.Store != nil && *req.Store)
	if len(tools) > 0 {
		body.Set("tools", wire.Array(tools...)).Set("tool_choice", responsesToolChoice(req.ToolChoice))
		body.SetBool("parallel_tool_calls", req.ParallelToolCalls == nil || *req.ParallelToolCalls)
	}
	if native {
		body.Set("context_management", req.ContextManagement)
	}
	scope := cacheScope(req)
	cacheKey := contentCacheKey(instructions, tools, scope)
	if cacheKey == "" {
		cacheKey = scope
	}
	if req.PromptCacheKey != "" {
		cacheKey = req.PromptCacheKey
	}
	if o.CacheKeyField && !xai && cacheKey != "" {
		body.SetString("prompt_cache_key", cacheKey)
	}
	switch {
	case req.CacheRetention != "":
		body.SetString("prompt_cache_retention", req.CacheRetention)
	case o.Backend == wire.ResponsesMeta,
		o.Backend == wire.ResponsesMantle && extendedCacheModel.MatchString(strings.ReplaceAll(strings.ToLower(strip(req.Model)), "_", "-")):
		body.SetString("prompt_cache_retention", "24h")
	}
	reasoningFields(body, req, effort, enabled, replay, enabled && o.AllTurns)
	if text := responsesText(req, xai); text != nil {
		body.Set("text", text)
	}
	if req.ServiceTier != "" {
		body.SetString("service_tier", req.ServiceTier)
	}
	if len(req.Include) > 0 {
		body.Set("include", wire.Strings(req.Include))
	}
	mergeInto(body, req.TopLevel)
	body.SetString("model", model)
	if body.Delete("prompt_cache_options") && p.cacheOptionsWarned.CompareAndSwap(false, true) {
		slog.Warn("dropped prompt_cache_options, the Responses wire takes it only inside the extra body")
	}
	if wire.IsAstraModel(req.Model) && o.Backend == wire.ResponsesOpenAI {
		sanitizeAstra(body)
	}
	boundCacheKeyField(body)
	if xai && !(isGrok46(req.Model) && body.Get("service_tier").Text() == "priority") {
		body.Delete("service_tier")
	}
	headers := map[string]string{}
	if codex {
		if req.SessionID != "" {
			headers["session_id"] = req.SessionID
		}
		requestID := body.Get("prompt_cache_key").Text()
		if requestID == "" {
			requestID = boundedCacheKey(scope)
		}
		if requestID != "" {
			headers["x-client-request-id"] = requestID
		}
	} else if req.MaxTokens > 0 {
		body.SetInt("max_output_tokens", req.MaxTokens)
	}
	extra := &wire.Dict{}
	mergeInto(extra, req.ExtraBody)
	if xai && req.SessionID != "" {
		headers["x-grok-conv-id"] = scope
		if !extra.Has("prompt_cache_key") {
			if key := body.Get("prompt_cache_key"); key.Exists() {
				extra.Set("prompt_cache_key", key.Raw())
			} else {
				extra.SetString("prompt_cache_key", cacheKey)
			}
		}
	}
	boundCacheKeyField(extra)
	normalized, err := p.preflight(body, req)
	if err != nil {
		return nil, err
	}
	boundCacheKeyField(normalized)
	if codex {
		for _, d := range [...]*wire.Dict{normalized, extra} {
			if d.Delete("prompt_cache_retention") {
				slog.Warn("dropped prompt_cache_retention, the ChatGPT Codex endpoint rejects it", "model", model)
			}
		}
	}
	for _, mb := range extra.Members() {
		normalized.Set(mb.Key, mb.Value)
	}
	if req.Stream {
		normalized.SetBool("stream", true)
	}
	return &wire.Call{
		Method: "POST",
		Path:   "/responses",
		Header: callHeader(req, headers),
		Body:   wire.ScrubSurrogates(normalized.JSON()),
		Stream: req.Stream,
		Decode: &responsesDecoder{aliases: aliases, issuer: o.IssuerKind, issuerModel: issuerModel, model: model,
			trusted: o.TrustsCompleted, noContinuation: o.NoContinuation},
	}, nil
}
