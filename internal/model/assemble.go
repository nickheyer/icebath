package model

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
	"github.com/nickheyer/icebath/internal/model/wire/gemini"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

const (
	defaultAPITimeout = 1800 * time.Second
	qwenCodeVersion   = "0.14.1"
	icebathReferer    = "https://icebath.app"
)

var (
	openRouterSorts        = map[string]bool{"throughput": true, "latency": true, "price": true}
	geminiNativeProviders  = map[string]bool{"gemini": true, "google": true, "google-gemini": true, "google-ai-studio": true}
	geminiSignatureHosts   = []string{"generativelanguage.googleapis.com", "aiplatform.googleapis.com"}
	mistralModelMarkers    = []string{"mistral", "magistral", "mixtral", "devstral", "codestral", "ministral", "pixtral", "voxtral"}
	developerRoleModels    = []string{"gpt-5", "codex"}
	maxCompletionPrefixes  = []string{"gpt-4o", "gpt-4.1", "gpt-5", "o1", "o3", "o4"}
	preserveDotsProviders  = []string{"alibaba", "minimax", "minimax-cn", "opencode-go", "opencode-zen", "zai", "bedrock", "xiaomi", "vertex"}
	knownRoutingProviders  = []string{"anthropic", "openai", "google", "openrouter", "groq", "mistral", "xai", "cohere", "perplexity", "together", "fireworks", "deepseek"}
	knownRoutingAggregates = []string{"openrouter", "opencode", "fireworks", "groq", "together"}
	dualSurfaceSuffixes    = []string{"minimax.io", "minimax.chat", "minimaxi.com"}
	nativeGeminiBodyKeys   = map[string]bool{"thinking_config": true, "thinkingConfig": true, "max_output_tokens": true}
)

func (s *Session) Prepare(ctx context.Context, req *wire.Request, turn TurnFacts) (*Prepared, error) {
	if req == nil {
		return nil, errors.New("model: prepare needs a request")
	}
	ctx = s.r.bind(ctx)
	s.refreshAnthropicBeforeCall(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if turn.SessionID != s.sessionID {
		s.sessionID = turn.SessionID
	}
	s.applyPendingWire()
	rt := s.route
	out := *req
	out.ExtraBody = maps.Clone(req.ExtraBody)
	out.TopLevel = maps.Clone(req.TopLevel)
	out.ExtraHeaders = maps.Clone(req.ExtraHeaders)
	msgs := cloneMessages(req.Messages)
	s.suppressRejectedThinking(ctx, msgs)
	replay := s.r.ReasoningReplayRoute(ctx, rt.Mode, rt.Provider, rt.Model, rt.BaseURL, s.echoOptIn, s.rejectedCarriersLocked(ctx))
	for i := range msgs {
		ApplyReasoningPolicy(&msgs[i], replay)
	}
	tools := req.Tools
	cached := false
	if !s.cacheDisabled && rt.Provider != "moa" {
		if cache, native := s.r.cachePolicy(ctx, cacheDestination{provider: rt.Provider, baseURL: rt.BaseURL, mode: rt.Mode, model: rt.Model}); cache {
			msgs, tools = s.r.planCache(msgs, tools, EffectiveCacheTTL(s.cacheTTL, rt.Model, rt.Provider), native,
				turn.StaticSystemPrefix, directNativeToolCache(rt.Mode, rt.BaseURL), EnvelopeToolPartMarkers(rt.Provider, rt.BaseURL))
			cached = true
		}
	}
	if !cached {
		msgs, tools = s.r.StripCacheDecoration(msgs, tools)
	}
	out.Messages = s.stripImagesForRejectingModel(msgs)
	out.Tools = tools
	out.Model = rt.Model
	out.SessionID = turn.SessionID
	timeout, err := s.requestTimeout(ctx)
	if err != nil {
		return nil, err
	}
	out.Timeout = timeout
	reasoning := s.reasoningForWire(ctx, turn.ReasoningOff)
	overrides, err := s.effectiveOverrides(ctx)
	if err != nil {
		return nil, err
	}
	maxTokens := s.o.MaxTokens
	if turn.EphemeralMaxTokens > 0 {
		maxTokens = turn.EphemeralMaxTokens
	}
	caller, injected, err := s.r.injectedCaller(ctx, rt)
	if err != nil {
		return nil, err
	}
	if injected {
		out.Reasoning, out.MaxTokens, out.Verbosity = reasoning, maxTokens, s.o.Verbosity
		for k, v := range overrides {
			out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{k: v})
		}
		out.ExtraHeaders = s.r.SessionAffinityHeaders(ctx, rt.Provider, rt.BaseURL, turn.SessionID, out.ExtraHeaders)
		return &Prepared{Mode: rt.Mode, Request: &out, Reasoning: reasoning, Caller: caller}, nil
	}
	var protocol wire.Protocol
	mode := rt.Mode
	switch rt.Mode {
	case wire.ModeAnthropic:
		protocol, err = s.assembleAnthropic(ctx, &out, reasoning, overrides, maxTokens, turn)
	case wire.ModeBedrock:
		protocol = s.assembleBedrock(&out, reasoning)
	case wire.ModeResponses:
		protocol, err = s.assembleResponses(ctx, &out, reasoning, overrides, maxTokens, turn)
	case wire.ModeChat:
		protocol, mode, err = s.assembleChat(ctx, &out, reasoning, overrides, maxTokens, turn)
	default:
		return nil, fmt.Errorf("model: route %s speaks %q, which no HTTP protocol builds", rt.Provider, rt.Mode)
	}
	if err != nil {
		return nil, err
	}
	out.ExtraHeaders = s.r.SessionAffinityHeaders(ctx, rt.Provider, rt.BaseURL, turn.SessionID, out.ExtraHeaders)
	if turn.EmptyRetries > 0 && providers.HostMatches(rt.BaseURL, "openrouter.ai") {
		setHeader(&out, "X-OpenRouter-Cache", "false")
	}
	if turn.UserInitiated {
		for k, v := range s.r.c.o.Providers.RequestHeaders(ctx, s.scope, rt.Provider, rt.BaseURL, rt.Model, true) {
			setHeader(&out, k, v)
		}
	}
	endpoint, err := s.endpoint(ctx, mode)
	if err != nil {
		return nil, err
	}
	call, err := protocol.Build(&out)
	if err != nil {
		return nil, err
	}
	return &Prepared{Mode: mode, Request: &out, Call: call, Endpoint: endpoint, Protocol: protocol, Reasoning: reasoning}, nil
}

func setHeader(out *wire.Request, key, value string) {
	if out.ExtraHeaders == nil {
		out.ExtraHeaders = map[string]string{}
	}
	out.ExtraHeaders[key] = value
}

func (s *Session) requestTimeout(ctx context.Context) (time.Duration, error) {
	if secs, ok := s.r.c.o.Config.ProviderRequestTimeout(ctx, s.route.Provider, s.route.Model); ok {
		return time.Duration(secs * float64(time.Second)), nil
	}
	raw, err := s.r.c.o.Secrets.Get(ctx, "ICEBATH_API_TIMEOUT", "")
	if err != nil {
		return 0, err
	}
	if raw = strings.TrimSpace(raw); raw != "" {
		if secs, perr := strconv.ParseFloat(raw, 64); perr == nil {
			return time.Duration(secs * float64(time.Second)), nil
		}
	}
	return defaultAPITimeout, nil
}

func dataUseOf(msgs []wire.Message) []string {
	seen := map[string]bool{}
	var out []string
	for i := range msgs {
		for _, d := range msgs[i].DataUse {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	slices.Sort(out)
	return out
}

func modelVariants(model string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(values ...string) {
		for _, v := range values {
			if v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	digitSwap := func(s string, from, to byte) string {
		b := []byte(s)
		for i := 1; i+1 < len(b); i++ {
			if b[i] == from && b[i-1] >= '0' && b[i-1] <= '9' && b[i+1] >= '0' && b[i+1] <= '9' {
				b[i] = to
				i++
			}
		}
		return string(b)
	}
	derive := func(s string) {
		dashed, dotted := strings.ReplaceAll(s, ".", "-"), strings.ReplaceAll(s, "-", ".")
		add(s, dashed, dotted, digitSwap(s, '-', '.'), digitSwap(s, '.', '-'), digitSwap(dashed, '-', '.'), digitSwap(dotted, '.', '-'))
	}
	derive(model)
	parts := strings.Split(model, "/")
	if len(parts) >= 2 {
		derive(parts[len(parts)-1])
	}
	if len(parts) >= 3 {
		derive(strings.Join(parts[1:], "/"))
	}
	for _, v := range slices.Clone(out) {
		if !strings.Contains(v, "/") {
			for _, p := range knownRoutingProviders {
				add(p + "/" + v)
			}
		}
	}
	for _, v := range slices.Clone(out) {
		if strings.Count(v, "/") == 1 {
			for _, a := range knownRoutingAggregates {
				add(a + "/" + v)
			}
		}
	}
	return out
}

func (s *Session) providerPreferences(ctx context.Context) (json.RawMessage, error) {
	flat := s.o.Routing
	cfg := s.r.frozenConfig()
	if cfg == nil {
		live, err := s.r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		cfg = live
	}
	if cfg.Extra.ProviderRouting != nil && s.route.Model != "" {
		for _, variant := range modelVariants(s.route.Model) {
			entry, ok := cfg.Extra.ProviderRouting.Models.Get(variant)
			if !ok {
				continue
			}
			if len(entry.Only) > 0 {
				flat.Only = entry.Only
			}
			if len(entry.Ignore) > 0 {
				flat.Ignore = entry.Ignore
			}
			if len(entry.Order) > 0 {
				flat.Order = entry.Order
			}
			if entry.Sort != "" {
				flat.Sort = entry.Sort
			}
			if entry.RequireParameters != nil {
				flat.RequireParameters = entry.RequireParameters
			}
			if entry.DataCollection != "" {
				flat.DataCollection = entry.DataCollection
			}
			break
		}
	}
	d := &wire.Dict{}
	if len(flat.Only) > 0 {
		d.Set("only", wire.Strings(flat.Only))
	}
	if len(flat.Ignore) > 0 {
		d.Set("ignore", wire.Strings(flat.Ignore))
	}
	if len(flat.Order) > 0 {
		d.Set("order", wire.Strings(flat.Order))
	}
	if sortValue := strings.ToLower(strings.TrimSpace(flat.Sort)); sortValue != "" {
		if openRouterSorts[sortValue] {
			d.SetString("sort", sortValue)
		} else {
			s.r.c.warnOnce("sort:"+flat.Sort, fmt.Sprintf("Ignoring invalid OpenRouter provider.sort value %q (allowed: latency, price, throughput)", flat.Sort))
		}
	}
	if flat.RequireParameters != nil && *flat.RequireParameters {
		d.SetBool("require_parameters", true)
	}
	if flat.DataCollection != "" {
		d.SetString("data_collection", flat.DataCollection)
	}
	if d.Len() == 0 {
		return nil, nil
	}
	return d.JSON(), nil
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Session) maxTokensField(ctx context.Context) wire.MaxTokensField {
	rt := s.route
	host := providers.URLHostname(rt.BaseURL)
	if host == "api.openai.com" || providers.HostMatches(strings.ToLower(rt.BaseURL), "openai.azure.com") {
		return wire.MaxCompletionTokens
	}
	if field, ok := s.r.c.o.Providers.MaxTokensField(ctx, s.scope, rt.Provider, rt.BaseURL, rt.Model); ok {
		return field
	}
	m := flatModel(rt.Model)
	for _, prefix := range maxCompletionPrefixes {
		if strings.HasPrefix(m, prefix) {
			return wire.MaxCompletionTokens
		}
	}
	return wire.MaxTokens
}

func keepsThoughtSignatures(ctx context.Context, reg *providers.Registry, model, baseURL, provider string) bool {
	if consumesThoughtSignature(model) {
		return true
	}
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "gemini" || name == "vertex" || anyHost(baseURL, geminiSignatureHosts) {
		return true
	}
	if name == "" {
		return false
	}
	p, ok := reg.Get(ctx, name)
	return ok && p.Spec().Name == name && p.Spec().ReplaysToolSignatures
}

func mistralToolIDs(provider, model, baseURL string) bool {
	if strings.ToLower(strings.TrimSpace(provider)) == "mistral" || providers.HostMatches(baseURL, "mistral.ai") {
		return true
	}
	lower := strings.ToLower(model)
	for _, marker := range mistralModelMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func developerRole(model string) bool {
	lower := strings.ToLower(model)
	for _, p := range developerRoleModels {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func (s *Session) carrierKeys(ctx context.Context) wire.CarrierKeys {
	var keys wire.CarrierKeys
	for _, p := range s.r.c.o.Providers.All(ctx) {
		sp := p.Spec()
		if sp.CarrierOpaque != "" && !slices.Contains(keys.Opaque, sp.CarrierOpaque) {
			keys.Opaque = append(keys.Opaque, sp.CarrierOpaque)
		}
		if sp.CarrierText != "" && !slices.Contains(keys.Text, sp.CarrierText) {
			keys.Text = append(keys.Text, sp.CarrierText)
		}
	}
	return keys
}

func mergeRaw(dst map[string]json.RawMessage, src map[string]json.RawMessage) map[string]json.RawMessage {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = map[string]json.RawMessage{}
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func objectMembers(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	v, err := wire.ParseValue(raw)
	if err != nil || !v.IsObject() {
		return nil, false
	}
	out := map[string]json.RawMessage{}
	for _, f := range v.Fields() {
		out[f.Key] = f.Value.Raw()
	}
	return out, true
}

func rawText(raw json.RawMessage) string {
	v, err := wire.ParseValue(raw)
	if err != nil {
		return ""
	}
	return v.Display()
}

func (s *Session) chatOptions(ctx context.Context, profile providers.Profile, provider string) wire.ChatOptions {
	rt := s.route
	o := wire.ChatOptions{
		DeveloperRole:   developerRole(rt.Model),
		KeepSignatures:  keepsThoughtSignatures(ctx, s.r.c.o.Providers, rt.Model, rt.BaseURL, provider),
		ReplayReasoning: s.r.RouteReasoningCarriers(ctx, provider, rt.BaseURL),
		MistralToolIDs:  mistralToolIDs(provider, rt.Model, rt.BaseURL),
		AliasToolSearch: rt.Provider == "xai" || rt.Provider == "xai-oauth" || providers.URLHostname(rt.BaseURL) == "api.x.ai",
		MoonshotSchemas: openai.IsMoonshotModel(rt.Model),
		Carriers:        s.carrierKeys(ctx),
	}
	if profile != nil {
		o.NativeDetailsType = profile.Spec().NativeDetailsType
		o.CacheKey = profile.Spec().PromptCacheKey
	} else {
		o.CacheKey = providers.URLHostname(rt.BaseURL) == "api.openai.com"
	}
	return o
}

func (s *Session) nativeGemini(profile providers.Profile) (*providers.GeminiClient, bool) {
	rt := s.route
	if profile != nil && profile.Spec().Gemini != nil {
		return profile.Spec().Gemini, true
	}
	if geminiNativeProviders[strings.ToLower(rt.Provider)] && gemini.IsNativeBaseURL(rt.BaseURL) {
		return &providers.GeminiClient{}, true
	}
	return nil, false
}

func (s *Session) assembleChat(ctx context.Context, out *wire.Request, reasoning *wire.Reasoning, overrides map[string]json.RawMessage, maxTokens int, turn TurnFacts) (wire.Protocol, wire.Mode, error) {
	rt := s.route
	scope := s.cacheScope(ctx, turn)
	out.CacheScope = scope
	dataUse := dataUseOf(out.Messages)
	out.Messages = s.shapeWireCarriers(ctx, out.Messages, rt.Model, rt.BaseURL)
	profile, hasProfile := s.r.c.o.Providers.Get(ctx, rt.Provider)
	if !hasProfile {
		profile = nil
	}
	providerName := rt.Provider
	if profile != nil {
		providerName = profile.Spec().Name
	}
	out.Chat = s.chatOptions(ctx, profile, providerName)
	prefs, err := s.providerPreferences(ctx)
	if err != nil {
		return nil, "", err
	}
	qwen := providers.HostMatches(rt.BaseURL, "portal.qwen.ai")
	var qwenMeta json.RawMessage
	if qwen {
		sid := turn.SessionID
		if sid == "" {
			sid = "icebath"
		}
		qwenMeta = (&wire.Dict{}).SetString("sessionId", sid).SetString("promptId", uuid4()).JSON()
	}
	supports := s.supportsReasoningExtraBody(ctx)
	clamped := wire.ClampReasoning(reasoning, wire.EffortsOpenAICompat.Efforts().Levels)
	field := s.maxTokensField(ctx)
	out.Reasoning = nil
	if profile != nil {
		if p, ok := profile.(providers.MessagePreparer); ok {
			prepared, err := p.PrepareMessages(ctx, out.Messages)
			if err != nil {
				return nil, "", err
			}
			out.Messages = prepared
		}
		sp := profile.Spec()
		switch {
		case sp.OmitTemperature:
			out.Temperature, out.OmitTemperature = nil, true
		case sp.FixedTemperature != nil:
			t := *sp.FixedTemperature
			out.Temperature = &t
		}
		profileMax, _ := providers.MaxTokensFor(profile, rt.Model)
		applyMaxTokens(out, rt.Model, clamped, maxTokens, profileMax, field)
		facts := providers.RouteFacts{
			Provider: rt.Provider, Model: rt.Model, BaseURL: rt.BaseURL, SessionID: turn.SessionID, CacheScope: scope,
			DataUse: dataUse, Reasoning: clamped, SupportsReasoning: supports, OllamaNumCtx: s.o.OllamaNumCtx,
			QwenSession: qwenMeta, ProviderPrefs: prefs, MinCodingScore: s.o.MinCodingScore,
		}
		if strings.ToLower(strings.TrimSpace(rt.Provider)) == "lmstudio" {
			facts.LMStudioOptions, facts.LMStudioProbed = s.lmStudioOptions(ctx), true
		}
		if caps, ok := s.reasoningCaps(ctx); ok {
			facts.Caps = &caps
		}
		var extra map[string]json.RawMessage
		if b, ok := profile.(providers.BodyExtender); ok {
			body, err := b.ExtraBody(ctx, facts)
			if err != nil {
				return nil, "", err
			}
			extra = mergeRaw(extra, body)
		}
		if a, ok := profile.(providers.ArgsExtender); ok {
			extras, err := a.APIExtras(ctx, facts)
			if err != nil {
				return nil, "", err
			}
			extra = mergeRaw(extra, extras.Body)
			out.TopLevel = mergeRaw(out.TopLevel, extras.TopLevel)
			for k, v := range extras.Headers {
				setHeader(out, k, v)
			}
			if extras.Model != "" {
				out.Model = extras.Model
			}
		}
		extra = mergeRaw(extra, out.ExtraBody)
		for k, v := range overrides {
			if k == "extra_body" {
				if members, ok := objectMembers(v); ok {
					extra = mergeRaw(extra, members)
					continue
				}
			}
			applyChatOverride(out, k, v)
		}
		if gc, ok := s.nativeGemini(profile); ok {
			return s.finishGemini(out, extra, gc)
		}
		out.ExtraBody = extra
		return openai.ChatProtocol{}, wire.ModeChat, nil
	}
	applyMaxTokens(out, rt.Model, clamped, maxTokens, 0, field)
	thinkingOff := clamped != nil && !clamped.Enabled
	effort := wire.RequestedEffort(clamped)
	kimi := anyHost(rt.BaseURL, []string{"api.kimi.com", "moonshot.ai", "moonshot.cn"})
	if kimi && !thinkingOff {
		family := wire.KimiEfforts(rt.Model)
		if effort == "" {
			def := "medium"
			if family == wire.EffortsKimiK3 {
				def = "high"
			}
			out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{"reasoning_effort": wire.Quote(def)})
		} else {
			out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{"reasoning_effort": wire.Quote(family.Efforts().Clamp(effort))})
		}
	}
	if providers.HostMatches(rt.BaseURL, "tokenhub.tencentmaas.com") && !thinkingOff {
		out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{"reasoning_effort": wire.Quote(wire.TokenHubEffort(effort))})
	}
	extra := map[string]json.RawMessage{}
	openRouter := providers.HostMatches(rt.BaseURL, "openrouter.ai")
	if openRouter && len(prefs) > 0 {
		extra["provider"] = prefs
	}
	if openRouter && rt.Model == "openrouter/pareto-code" {
		if score, ok := paretoScore(s.o.MinCodingScore); ok {
			plugin := (&wire.Dict{}).SetString("id", "pareto-router").SetFloat("min_coding_score", score)
			extra["plugins"] = wire.Array(plugin.JSON())
		}
	}
	if kimi {
		state := "enabled"
		if thinkingOff {
			state = "disabled"
		}
		extra["thinking"] = (&wire.Dict{}).SetString("type", state).JSON()
	}
	if supports {
		if _, claimed := s.reasoningEndpointProfile(ctx); claimed {
			if payload, ok := s.endpointReasoningPayload(ctx, s.o.Reasoning); ok {
				extra["reasoning"] = payload
			}
		} else {
			level := "medium"
			if clamped != nil && clamped.Effort != "" {
				level = clamped.Effort
			}
			off := thinkingOff || level == "none"
			if off {
				level = "none"
			}
			extra["reasoning"] = (&wire.Dict{}).SetBool("enabled", !off).SetString("effort", level).JSON()
		}
	}
	if strings.ToLower(strings.TrimSpace(rt.Provider)) == "gemini" {
		raw := gemini.ThinkingConfig(rt.Model, clamped)
		if gemini.IsOpenAICompatBaseURL(rt.BaseURL) {
			if snake := gemini.SnakeThinkingConfig(raw); len(snake) > 0 {
				nested := &wire.Dict{}
				if v, err := wire.ParseValue(extra["extra_body"]); err == nil && v.IsObject() {
					nested = wire.DictOf(v)
				}
				google := &wire.Dict{}
				if v := nested.Get("google"); v.IsObject() {
					google = wire.DictOf(v)
				}
				google.Set("thinking_config", snake)
				nested.Set("google", google.JSON())
				extra["extra_body"] = nested.JSON()
			}
		} else if len(raw) > 0 {
			extra["thinking_config"] = raw
		}
	}
	extra = mergeRaw(extra, out.ExtraBody)
	out.ExtraBody = extra
	for k, v := range overrides {
		if k == "extra_body" {
			if members, ok := objectMembers(v); ok {
				out.ExtraBody = members
				continue
			}
		}
		applyChatOverride(out, k, v)
	}
	if gc, ok := s.nativeGemini(nil); ok {
		return s.finishGemini(out, out.ExtraBody, gc)
	}
	return openai.ChatProtocol{}, wire.ModeChat, nil
}

func (s *Session) reasoningCaps(ctx context.Context) (providers.ReasoningCaps, bool) {
	rt := s.route
	switch {
	case providers.HostMatches(rt.BaseURL, "openrouter.ai"):
		return s.r.c.o.Catalog.OpenRouterReasoningCaps(ctx, rt.Model, false)
	case portalRoute(rt):
		return s.r.c.o.Catalog.PortalReasoningCaps(ctx, rt.Model, false)
	}
	return providers.ReasoningCaps{}, false
}

func paretoScore(raw json.RawMessage) (float64, bool) {
	v, err := wire.ParseValue(raw)
	if err != nil || v.Kind() <= wire.KindNull {
		return 0, false
	}
	score, ok := v.Float()
	if !ok {
		text := strings.TrimSpace(v.Text())
		if text == "" {
			return 0, false
		}
		f, perr := strconv.ParseFloat(text, 64)
		if perr != nil {
			return 0, false
		}
		score = f
	}
	return score, score >= 0 && score <= 1
}

func applyMaxTokens(out *wire.Request, model string, r *wire.Reasoning, requested, profileMax int, field wire.MaxTokensField) {
	n := requested
	if n <= 0 {
		n = profileMax
	}
	if n <= 0 {
		out.MaxTokens = 0
		return
	}
	out.MaxTokens = gemini.RaiseMaxTokens(model, r, n)
	out.MaxTokensField = field
}

func applyChatOverride(out *wire.Request, key string, value json.RawMessage) {
	switch key {
	case "model":
		out.Model = rawText(value)
	case "service_tier":
		out.ServiceTier = rawText(value)
	default:
		out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{key: value})
	}
}

func (s *Session) finishGemini(out *wire.Request, extra map[string]json.RawMessage, gc *providers.GeminiClient) (wire.Protocol, wire.Mode, error) {
	kept := map[string]json.RawMessage{}
	for k, v := range extra {
		if nativeGeminiBodyKeys[k] {
			kept[k] = v
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	out.ExtraBody = kept
	o := gc.Options
	base := s.route.BaseURL
	if base == "" {
		base = gc.DefaultBaseURL
	}
	o.BaseURL = gemini.NormalizeBaseURL(base)
	o.KeyKind = gemini.KeyKind(keyText(s.route.Credential))
	out.Gemini = o
	return gemini.Protocol{Version: s.r.c.o.Version}, wire.ModeChat, nil
}

func (s *Session) responsesBackend() wire.ResponsesBackend {
	rt := s.route
	switch rt.Provider {
	case "openai-codex":
		return wire.ResponsesCodex
	case "xai", "xai-oauth":
		return wire.ResponsesXAI
	case providers.ProviderAzureFoundry:
		return wire.ResponsesAzureFoundry
	}
	return openai.BackendForHost(rt.BaseURL)
}

func (s *Session) codexBackend() bool {
	rt := s.route
	host := providers.URLHostname(rt.BaseURL)
	return rt.Provider == "openai-codex" || ((host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")) &&
		strings.Contains(strings.ToLower(rt.BaseURL), "/backend-api/codex"))
}

func (s *Session) searchProvider(ctx context.Context) (string, error) {
	return s.r.c.o.SearchProvider(ctx)
}

func (s *Session) assembleResponses(ctx context.Context, out *wire.Request, reasoning *wire.Reasoning, overrides map[string]json.RawMessage, maxTokens int, turn TurnFacts) (wire.Protocol, error) {
	rt := s.route
	codex := s.codexBackend()
	xai := rt.Provider == "xai" || rt.Provider == "xai-oauth" || providers.URLHostname(rt.BaseURL) == "api.x.ai"
	backend := s.responsesBackend()
	var quirks providers.ResponsesQuirks
	claimed, claimedOK := s.r.c.o.Providers.ForHost(ctx, rt.BaseURL, false)
	if claimedOK {
		quirks = claimed.Spec().Responses
	}
	out.CacheScope = s.cacheScope(ctx, turn)
	wireModel := rt.Model
	if m := overrideText(overrides, "model"); m != "" {
		wireModel = m
	}
	wireModel = strings.TrimSpace(wireModel)
	o := wire.ResponsesOptions{
		Backend:          backend,
		ReplayItemIDs:    !quirks.DropItemIDs,
		CacheKeyField:    !quirks.NoPromptCacheKey,
		TrustsCompleted:  !claimedOK || !quirks.DistrustsCompleted,
		NativeCompaction: !quirks.NoNativeCompaction,
		ProfileReasoning: quirks.ProfileReasoning,
		IssuerKind:       openai.IssuerKind(backend, quirks.IssuerKind, rt.BaseURL),
		IssuerModel:      stripContextVariant(wireModel),
		ReplayEncrypted:  !s.o.NoEncryptedReplay,
		NewestOnly:       rt.Provider == providers.ProviderAzureFoundry || providers.HostMatches(rt.BaseURL, "openai.azure.com") || providers.HostMatches(rt.BaseURL, "services.ai.azure.com"),
		SanitizeHarmony:  codex && rt.Mode == wire.ModeResponses,
		Identity:         s.o.Identity,
		Prune:            s.o.Prune,
		RasterizeSVG:     s.o.RasterizeSVG,
	}
	if p, ok := s.r.c.o.Providers.Get(ctx, rt.Provider); ok {
		if d, ok := p.(providers.EffortDeclarer); ok {
			if efforts, declared := d.Efforts(ctx, rt.Model); declared {
				o.Efforts = efforts
			}
		}
	}
	o.AllTurns = !codex && providers.URLHostname(rt.BaseURL) == "api.openai.com" && openai.ReasoningFamily(stripContextVariant(wireModel)) != "" &&
		!s.allTurnsRejected[stripContextVariant(wireModel)]
	switch {
	case xai:
		name, err := s.searchProvider(ctx)
		o.NativeWebSearch = err != nil || name == "xai"
	case codex:
		name, err := s.searchProvider(ctx)
		o.NativeWebSearch = err == nil && name == "openai-native"
	}
	out.Responses = o
	out.ContextManagement = s.contextManagement(turn, codex, xai, o.NativeCompaction)
	out.Reasoning = reasoning
	if o.ProfileReasoning {
		payload, _ := s.endpointReasoningPayload(ctx, s.o.Reasoning)
		if out.Reasoning == nil {
			out.Reasoning = &wire.Reasoning{Enabled: true}
		} else {
			c := *out.Reasoning
			out.Reasoning = &c
		}
		out.Reasoning.Payload = payload
	}
	out.MaxTokens = maxTokens
	out.Verbosity = s.o.Verbosity
	for k, v := range overrides {
		switch k {
		case "model":
		case "service_tier":
			out.ServiceTier = rawText(v)
		case "extra_body":
			if members, ok := objectMembers(v); ok {
				out.ExtraBody = mergeRaw(out.ExtraBody, members)
				continue
			}
			out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{k: v})
		default:
			out.TopLevel = mergeRaw(out.TopLevel, map[string]json.RawMessage{k: v})
		}
	}
	out.Model = stripContextVariant(wireModel)
	return s.r.c.responses, nil
}

func stripContextVariant(model string) string {
	return catalog.StripCodexContextVariantSuffix(model)
}

func (s *Session) preserveDots() bool {
	rt := s.route
	if slices.Contains(preserveDotsProviders, strings.ToLower(rt.Provider)) {
		return true
	}
	base := strings.ToLower(rt.BaseURL)
	host := providers.URLHostname(base)
	return strings.Contains(host, "dashscope") || providers.HostMatches(base, "aliyuncs.com") || strings.Contains(host, "minimax") ||
		(providers.HostMatches(base, "opencode.ai") && strings.Contains(base, "/zen/")) || providers.HostMatches(base, "bigmodel.cn") ||
		providers.HostMatches(base, "xiaomimimo.com") || providers.HostMatches(base, "aiplatform.googleapis.com") ||
		strings.HasPrefix(host, "bedrock-runtime.")
}

func (s *Session) assembleAnthropic(ctx context.Context, out *wire.Request, reasoning *wire.Reasoning, overrides map[string]json.RawMessage, maxTokens int, turn TurnFacts) (wire.Protocol, error) {
	rt := s.route
	base := s.anthropicBaseURL()
	portal := portalRoute(rt)
	out.MaxTokens = maxTokens
	out.Reasoning = reasoning
	out.Anthropic = wire.AnthropicOptions{
		OAuth:             anthropic.Style(keyText(rt.Credential), base, portal) == anthropic.StyleOAuth,
		PreserveDots:      s.preserveDots(),
		KeepCatalogIDs:    portal,
		ContextLength:     turn.ContextLength,
		FastMode:          overrideText(overrides, "speed") == "fast",
		DropContext1MBeta: s.drop1MBeta,
		InvokeModel:       rt.BedrockAnthropic,
		BaseURL:           base,
		BufferToolInput:   s.bufferToolInput,
		RasterizeSVG:      s.o.RasterizeSVG,
	}
	if rt.BedrockAnthropic {
		out.Anthropic.BaseURL = rt.BaseURL
		out.Bedrock.Guardrail = rt.Guardrail
		out.Bedrock.Region = rt.Region
	}
	if rt.Provider == "portal" || rt.Provider == "icebath-portal" {
		if err := s.mergePortalBody(ctx, out, turn); err != nil {
			return nil, err
		}
	}
	return s.r.c.anthropic, nil
}

func (s *Session) mergePortalBody(ctx context.Context, out *wire.Request, turn TurnFacts) error {
	p, ok := s.r.c.o.Providers.Get(ctx, "portal")
	if !ok {
		return nil
	}
	b, ok := p.(providers.BodyExtender)
	if !ok {
		return nil
	}
	body, err := b.ExtraBody(ctx, providers.RouteFacts{Provider: "portal", Model: s.route.Model, BaseURL: s.route.BaseURL,
		SessionID: turn.SessionID, DataUse: dataUseOf(out.Messages)})
	if err != nil {
		return err
	}
	out.ExtraBody = mergeRaw(out.ExtraBody, body)
	return nil
}

func (s *Session) assembleBedrock(out *wire.Request, reasoning *wire.Reasoning) wire.Protocol {
	rt := s.route
	region := rt.Region
	if region == "" {
		region = "us-east-1"
	}
	out.MaxTokens = s.o.MaxTokens
	out.Reasoning = reasoning
	out.Bedrock = wire.BedrockOptions{
		Region:         region,
		Guardrail:      rt.Guardrail,
		CachePoints:    !IsOneShotRequest(out.Messages, out.Tools),
		ResolveProfile: s.r.c.o.InferenceProfile,
	}
	return s.r.c.bedrock
}

func toOpenAIBaseURL(baseURL string) string {
	url := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if providers.URLHostname(url) == "api.actual.inc" {
		return auth.NormalizeActualBaseURL(url)
	}
	if strings.HasSuffix(url, "/anthropic") {
		if providers.HostMatches(url, "open.bigmodel.cn") || providers.HostMatches(url, "api.z.ai") {
			return strings.TrimSuffix(url, "/anthropic") + "/coding/paas/v4"
		}
		if dualSurfaceHost(url) {
			return strings.TrimSuffix(url, "/anthropic") + "/v1"
		}
		return url
	}
	if providers.HostMatches(url, "api.kimi.com") && strings.HasSuffix(url, "/coding") {
		return url + "/v1"
	}
	return url
}

func dualSurfaceHost(url string) bool {
	host := providers.URLHostname(url)
	for _, suffix := range dualSurfaceSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return strings.HasPrefix(host, "api.minimax.")
}

type bearerAuth struct {
	key    auth.Secret
	tokens auth.TokenSource
}

func (b bearerAuth) Authorize(ctx context.Context, r *http.Request, _ []byte) error {
	key := b.key
	if b.tokens != nil {
		t, err := b.tokens.Token(ctx)
		if err != nil {
			return err
		}
		key = t.Value
	}
	if !key.Empty() {
		r.Header.Set("Authorization", "Bearer "+key.Reveal())
	}
	return nil
}

func (s *Session) tokenMint() func(ctx context.Context) (string, error) {
	tokens := s.route.Credential.Tokens
	return func(ctx context.Context) (string, error) {
		t, err := tokens.Token(ctx)
		if err != nil {
			return "", err
		}
		return t.Value.Reveal(), nil
	}
}

func (s *Session) authorizer(mode wire.Mode, baseURL string, nativeGemini bool) wire.Authorizer {
	cred := s.route.Credential
	switch {
	case cred.Kind == auth.CredSigned && cred.Sign != nil:
		return cred.Sign
	case cred.Kind == auth.CredToken && cred.Tokens != nil && (cred.Source == "entra_id" || s.route.AuthMode == "entra_id"):
		return providers.EntraAuthorizer(cred.Tokens, nil)
	case mode == wire.ModeAnthropic && cred.Kind == auth.CredToken && cred.Tokens != nil:
		return anthropic.TokenAuth(s.tokenMint(), baseURL)
	case mode == wire.ModeAnthropic:
		return anthropic.KeyAuth(keyText(cred), baseURL, portalRoute(s.route))
	case nativeGemini && cred.Kind != auth.CredToken:
		return gemini.APIKey(keyText(cred))
	case cred.Kind == auth.CredToken && cred.Tokens != nil:
		return bearerAuth{tokens: cred.Tokens}
	}
	return bearerAuth{key: cred.Key}
}

func (s *Session) routeDefaultHeaders(ctx context.Context, baseURL string) (map[string]string, bool, error) {
	version := s.r.c.o.Version
	agent := httpclient.UserAgent(version)
	switch {
	case providers.HostMatches(baseURL, "openrouter.ai"):
		h, err := s.openRouterHeaders(ctx)
		return h, true, err
	case providers.HostMatches(baseURL, "ai-gateway.vercel.sh"), providers.HostMatches(baseURL, "api.kimi.com"):
		return map[string]string{"HTTP-Referer": icebathReferer, "X-Title": "icebath", "User-Agent": agent}, true, nil
	case providers.HostMatches(baseURL, "integrate.api.nvidia.com"):
		return map[string]string{"X-BILLING-INVOKE-ORIGIN": "icebath"}, true, nil
	case providers.HostMatches(baseURL, "api.routermint.com"):
		return map[string]string{"User-Agent": agent}, true, nil
	case providers.HostMatches(baseURL, "portal.qwen.ai"):
		ua := fmt.Sprintf("QwenCode/%s (%s; %s)", qwenCodeVersion, runtime.GOOS, machineName())
		return map[string]string{"User-Agent": ua, "X-DashScope-CacheControl": "enable", "X-DashScope-UserAgent": ua,
			"X-DashScope-AuthType": "qwen-oauth"}, true, nil
	case providers.HostMatches(baseURL, "chatgpt.com"):
		token, err := s.codexToken(ctx)
		if err != nil {
			return nil, true, err
		}
		out := map[string]string{}
		for k, v := range openai.CodexHeaders(token.Reveal(), baseURL, version) {
			if len(v) > 0 {
				out[k] = v[0]
			}
		}
		return out, true, nil
	case providers.HostMatches(baseURL, "x.ai"):
		return map[string]string{"User-Agent": agent}, true, nil
	}
	return nil, false, nil
}

func machineName() string {
	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		return "AMD64"
	case runtime.GOOS == "windows" && runtime.GOARCH == "arm64":
		return "ARM64"
	case runtime.GOARCH == "amd64":
		return "x86_64"
	case runtime.GOARCH == "arm64" && runtime.GOOS == "linux":
		return "aarch64"
	}
	return runtime.GOARCH
}

func (s *Session) openRouterHeaders(ctx context.Context) (map[string]string, error) {
	h := map[string]string{"HTTP-Referer": icebathReferer, "X-Title": "icebath", "X-OpenRouter-Categories": "productivity,cli-agent"}
	cfg := s.r.frozenConfig()
	if cfg == nil {
		live, err := s.r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		cfg = live
	}
	envCache, err := s.r.c.o.Secrets.Get(ctx, "ICEBATH_OPENROUTER_CACHE", "")
	if err != nil {
		return nil, err
	}
	envCache = strings.ToLower(strings.TrimSpace(envCache))
	on := cfg.OpenRouter.ResponseCache
	if envCache != "" {
		on = envCache == "1" || envCache == "true" || envCache == "yes" || envCache == "on"
	}
	if !on {
		return h, nil
	}
	h["X-OpenRouter-Cache"] = "true"
	envTTL, err := s.r.c.o.Secrets.Get(ctx, "ICEBATH_OPENROUTER_CACHE_TTL", "")
	if err != nil {
		return nil, err
	}
	if envTTL = strings.TrimSpace(envTTL); envTTL != "" {
		if n, perr := strconv.Atoi(envTTL); perr == nil && !strings.ContainsAny(envTTL, "+-") && n >= 1 && n <= 86400 {
			h["X-OpenRouter-Cache-TTL"] = strconv.Itoa(n)
		}
		return h, nil
	}
	ttl := cfg.OpenRouter.ResponseCacheTTL
	if ttl == 0 {
		ttl = 300
	}
	if ttl >= 1 && ttl <= 86400 {
		h["X-OpenRouter-Cache-TTL"] = strconv.Itoa(ttl)
	}
	return h, nil
}

func (s *Session) userDefaultHeaders(ctx context.Context) (map[string]string, error) {
	cfg := s.r.frozenConfig()
	if cfg == nil {
		live, err := s.r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		cfg = live
	}
	out := map[string]string{}
	for _, k := range cfg.Model.DefaultHeaders.Keys() {
		v, _ := cfg.Model.DefaultHeaders.Get(k)
		out[k] = v
	}
	for _, k := range cfg.Model.ExtraHeaders.Keys() {
		v, _ := cfg.Model.ExtraHeaders.Get(k)
		out[k] = v
	}
	return out, nil
}

func setMissing(h http.Header, extra map[string]string) {
	for k, v := range extra {
		if h.Get(k) == "" && len(h.Values(k)) == 0 {
			h.Set(k, v)
		}
	}
}

func (s *Session) endpointHeaders(ctx context.Context, mode wire.Mode, baseURL string) (http.Header, error) {
	rt := s.route
	h := http.Header{}
	for k, v := range rt.Headers {
		h.Set(k, v)
	}
	if mode == wire.ModeAnthropic || mode == wire.ModeBedrock {
		return h, nil
	}
	key := rt.Credential.Key
	if rt.Credential.Kind != auth.CredAPIKey && rt.Credential.Kind != auth.CredXAPIKey {
		key = auth.Secret{}
	}
	routeHeaders, matched, err := s.routeDefaultHeaders(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	if !matched {
		routeHeaders = s.r.c.o.Providers.ClientHeaders(ctx, s.scope, rt.Provider, baseURL, key, false)
		if len(routeHeaders) == 0 {
			if p, ok := s.r.c.o.Providers.Get(ctx, rt.Provider); ok && len(p.Spec().DefaultHeaders) > 0 {
				routeHeaders = p.Spec().DefaultHeaders
			}
		}
	}
	for k, v := range routeHeaders {
		h.Set(k, v)
	}
	user, err := s.userDefaultHeaders(ctx)
	if err != nil {
		return nil, err
	}
	for k, v := range user {
		h.Set(k, v)
	}
	for k, v := range s.r.EndpointHeaders(ctx, baseURL) {
		h.Set(k, v)
	}
	setMissing(h, s.r.c.o.Providers.ClientHeaders(ctx, s.scope, rt.Provider, baseURL, key, false))
	if openai.IsOfficialCodexURL(baseURL) {
		token, err := s.codexToken(ctx)
		if err != nil {
			return nil, err
		}
		openai.RequireCodexHeaders(h, token.Reveal(), baseURL, s.r.c.o.Version)
	}
	return h, nil
}

func (s *Session) endpoint(ctx context.Context, mode wire.Mode) (wire.Endpoint, error) {
	rt := s.route
	base := rt.BaseURL
	nativeGemini := false
	switch {
	case rt.Mode == wire.ModeAnthropic && !rt.BedrockAnthropic:
		base = anthropic.NormalizeBaseURL(rt.BaseURL)
	case rt.Mode == wire.ModeAnthropic, rt.Mode == wire.ModeBedrock:
	default:
		p, ok := s.r.c.o.Providers.Get(ctx, rt.Provider)
		if !ok {
			p = nil
		}
		if gc, isNative := s.nativeGemini(p); isNative && rt.Mode == wire.ModeChat {
			nativeGemini = true
			if base == "" {
				base = gc.DefaultBaseURL
			}
			base = gemini.NormalizeBaseURL(base)
		} else {
			base = toOpenAIBaseURL(rt.BaseURL)
		}
	}
	header, err := s.endpointHeaders(ctx, mode, base)
	if err != nil {
		return wire.Endpoint{}, err
	}
	client, err := s.r.endpointClient(ctx, rt.BaseURL)
	if err != nil {
		return wire.Endpoint{}, err
	}
	return wire.Endpoint{BaseURL: base, Auth: s.authorizer(rt.Mode, base, nativeGemini), Header: header, Client: client}, nil
}

func (r *Router) endpointClient(ctx context.Context, baseURL string) (*http.Client, error) {
	tlsOpts, set := r.EndpointTLS(ctx, baseURL)
	if !set {
		return r.c.o.HTTP.Client(), nil
	}
	key := tlsOpts.RootCAFile + "\x00" + strconv.FormatBool(tlsOpts.Insecure)
	r.c.mu.Lock()
	t, ok := r.c.transports[key]
	r.c.mu.Unlock()
	if ok {
		return t.Client(), nil
	}
	o := r.c.o.HTTPOptions
	if tlsOpts.Insecure {
		o.TLS.Insecure = true
		r.c.warnOnce("insecure:"+baseURL, "TLS certificate verification DISABLED (ssl_verify: false), intended for local development only and unsafe on any network you do not fully control")
	} else if tlsOpts.RootCAFile != "" {
		o.TLS.RootCAFile = tlsOpts.RootCAFile
	}
	built, err := httpclient.New(o)
	if err != nil {
		return nil, err
	}
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	if existing, ok := r.c.transports[key]; ok {
		built.CloseIdleConnections()
		return existing.Client(), nil
	}
	r.c.transports[key] = built
	return built.Client(), nil
}
