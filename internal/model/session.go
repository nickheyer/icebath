package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
	"github.com/nickheyer/icebath/internal/model/wire/openai"
)

const (
	probeTTL                 = 60 * time.Second
	defaultFastWindow        = 60 * time.Second
	codexTierTTL             = time.Hour
	reasoningFloorEffort     = "low"
	rejectedThinkingKey      = "_anthropic_rejected_thinking"
	declaredScopePrefix      = "gwk_"
	ForkScopeSeparator       = "::"
	daybreakProgram          = "daybreak_blue"
	localTriggerSafetyMargin = 8192
	defaultCompactThreshold  = 200000
	gmiNativeWireCleared     = false
)

var (
	openRouterReasoningPrefixes = []string{"deepseek/", "anthropic/", "openai/", "x-ai/", "google/gemini-2", "google/gemma-4",
		"qwen/qwen3", "tencent/hy", "xiaomi/"}
	coreReasoningGatedHosts = []string{"icebath.app", "ai-gateway.vercel.sh", "ollama.com"}
	boundedTierModes        = map[string]bool{"auto": true, "cold": true}
	fastLimitHeaders        = []string{"anthropic-fast-input-tokens-limit", "anthropic-fast-output-tokens-limit"}
	openRouterGenerationID  = regexp.MustCompile(`^gen-\d{9,}-[A-Za-z0-9_-]{8,}$`)
	grokAggregatorPrefixes  = []string{"x-ai/grok-", "xai/grok-"}
)

type SessionStore interface {
	ModelConfigValue(ctx context.Context, sessionID, key string) (json.RawMessage, bool, error)
	PatchModelConfig(ctx context.Context, sessionID string, patch map[string]json.RawMessage) error
	CompressionLineage(ctx context.Context, sessionID string) ([]string, error)
	ScopeIdentity(ctx context.Context, sessionID string) (fork bool, source string, err error)
	ConversationGeneration(ctx context.Context, key, source string) (int64, bool, error)
	UpdateBillingRoute(ctx context.Context, sessionID, provider, baseURL string, mode wire.Mode) error
}

type SessionOptions struct {
	Store               SessionStore
	PersistDisabled     bool
	GatewaySessionKey   string
	InheritedCacheScope string
	ForkTag             string
	Source              string
	Reasoning           *wire.Reasoning
	ServiceTier         string
	FastAutoSeconds     *float64
	Daybreak            bool
	Overrides           map[string]json.RawMessage
	MaxTokens           int
	Verbosity           string
	Routing             config.ProviderRouting
	MinCodingScore      json.RawMessage
	NativeCompaction    bool
	CompressionDisabled bool
	CheckpointRequired  bool
	CompactThreshold    int
	NoEncryptedReplay   bool
	Identity            string
	Prune               wire.InputPruner
	RasterizeSVG        func(dataURL string) (png string, ok bool)
	OllamaNumCtx        int
	Fallback            []config.FallbackEntry
}

type TurnFacts struct {
	SessionID          string
	UserInitiated      bool
	EphemeralMaxTokens int
	ReasoningOff       bool
	StaticSystemPrefix string
	ContextLength      int
	LocalTriggerTokens int
	CompressionCount   int
	EmptyRetries       int
}

type Prepared struct {
	Mode      wire.Mode
	Request   *wire.Request
	Call      *wire.Call
	Endpoint  wire.Endpoint
	Protocol  wire.Protocol
	Reasoning *wire.Reasoning
	Caller    wire.Caller
	Sent      *wire.Call
}

type probeKey struct{ model, baseURL string }

type probeEntry struct {
	options []string
	known   bool
	value   bool
	at      time.Time
}

type Session struct {
	r     *Router
	o     SessionOptions
	scope *providers.Scope

	mu                sync.Mutex
	route             *Route
	sessionID         string
	echoOptIn         bool
	cacheDisabled     bool
	cacheTTL          string
	lmOptions         map[probeKey]probeEntry
	ollamaThinking    map[probeKey]probeEntry
	rejectedCarriers  map[reasoningRouteKey]wire.ReasoningKeys
	carriersLoadedFor string
	thinkingFor       string
	thinkingBound     bool
	thinking          map[string]bool
	imageRejecting    map[[2]string]bool
	fastUntil         time.Time
	fastUnavailable   map[string]bool
	effortRejected    bool
	disableRejected   bool
	floorRequired     bool
	wireReasoning     *wire.Reasoning
	portalDecided     bool
	portalPending     wire.Mode
	portalUpstream    string
	allTurnsRejected  map[string]bool
	drop1MBeta        bool
	bufferToolInput   bool
	scopeMemoSet      bool
	scopeMemoSID      string
	scopeMemoDB       bool
	scopeMemo         string
	exec              execState
	rec               recovery
}

func (r *Router) Open(ctx context.Context, route *Route, o SessionOptions) (*Session, error) {
	if route == nil {
		return nil, errors.New("model: a session needs a resolved route")
	}
	ctx = r.bind(ctx)
	cfg := r.frozenConfig()
	if cfg == nil {
		live, err := r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return nil, err
		}
		cfg = live
	}
	s := &Session{
		r: r, o: o, scope: providers.NewScope(), route: route,
		echoOptIn:        cfg.Model.ReasoningEcho != nil && *cfg.Model.ReasoningEcho,
		cacheDisabled:    CacheTTLDisabled(cfg.PromptCaching.CacheTTL),
		cacheTTL:         configuredCacheTTL(cfg.PromptCaching.CacheTTL, o.Source),
		lmOptions:        map[probeKey]probeEntry{},
		ollamaThinking:   map[probeKey]probeEntry{},
		rejectedCarriers: map[reasoningRouteKey]wire.ReasoningKeys{},
		thinking:         map[string]bool{},
		imageRejecting:   map[[2]string]bool{},
		fastUnavailable:  map[string]bool{},
		allTurnsRejected: map[string]bool{},
	}
	s.initRecovery(ctx)
	return s, nil
}

func (r *Router) frozenConfig() *config.Config {
	if r.frozen != nil {
		return r.frozen.cfg
	}
	return nil
}

func (s *Session) Route() *Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.route
}

func (s *Session) Bind(route *Route) {
	if route == nil {
		return
	}
	s.exec.resetStreak()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.route = route
}

func (s *Session) SetReasoningEcho(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.echoOptIn = on
}

func (s *Session) ReasoningEcho() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.echoOptIn
}

func (s *Session) RejectReasoningEffort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effortRejected = true
}

func (s *Session) RejectReasoningDisable(floor bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.disableRejected = true
	s.floorRequired = s.floorRequired || floor
}

func (s *Session) WireReasoning() *wire.Reasoning {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wireReasoning
}

func (s *Session) RejectAllTurns(model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.allTurnsRejected[model] {
		return false
	}
	s.allTurnsRejected[model] = true
	return true
}

func (s *Session) DisableContext1MBeta() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drop1MBeta = true
}

func (s *Session) BufferToolInput() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bufferToolInput = true
}

func (s *Session) probeKeyOf(ctx context.Context) local.ProbeKey {
	cred := s.route.Credential
	key := local.ProbeKey{Static: keyText(cred)}
	if cred.Kind == auth.CredToken && cred.Tokens != nil {
		tokens := cred.Tokens
		key.Mint = func(ctx context.Context) (string, error) {
			t, err := tokens.Token(ctx)
			if err != nil {
				return "", err
			}
			return t.Value.Reveal(), nil
		}
	}
	return key
}

func (s *Session) lmStudioOptions(ctx context.Context) []string {
	k := probeKey{s.route.Model, s.route.BaseURL}
	if e, ok := s.lmOptions[k]; ok && (len(e.options) > 0 || time.Since(e.at) < probeTTL) {
		return e.options
	}
	opts := s.r.c.o.Local.LMStudioReasoningOptions(ctx, s.route.Model, s.route.BaseURL, s.probeKeyOf(ctx), 0)
	s.lmOptions[k] = probeEntry{options: opts, at: time.Now()}
	return opts
}

func (s *Session) ollamaSupportsThinking(ctx context.Context) bool {
	k := probeKey{s.route.Model, s.route.BaseURL}
	if e, ok := s.ollamaThinking[k]; ok && (e.known || time.Since(e.at) < probeTTL) {
		return e.value
	}
	value, known := s.r.c.o.Local.OllamaModelSupportsThinking(ctx, s.route.Model, s.route.BaseURL, s.probeKeyOf(ctx), 0)
	s.ollamaThinking[k] = probeEntry{value: value && known, known: known, at: time.Now()}
	return value && known
}

func (s *Session) reasoningEndpointProfile(ctx context.Context) (providers.Profile, bool) {
	rt := s.route
	if strings.ToLower(strings.TrimSpace(rt.Provider)) == "lmstudio" || providers.HostMatches(rt.BaseURL, "openrouter.ai") ||
		anyHost(rt.BaseURL, coreReasoningGatedHosts) {
		return nil, false
	}
	return s.r.c.o.Providers.ForHost(ctx, rt.BaseURL, false)
}

func (s *Session) endpointReasoningPayload(ctx context.Context, rc *wire.Reasoning) (json.RawMessage, bool) {
	p, ok := s.reasoningEndpointProfile(ctx)
	if !ok {
		return nil, false
	}
	return s.r.c.o.Providers.ReasoningPayload(ctx, s.scope, p, s.route.Model, rc, s.route.BaseURL)
}

func (s *Session) supportsReasoningExtraBody(ctx context.Context) bool {
	rt := s.route
	if providers.HostMatches(rt.BaseURL, "icebath.app") || providers.HostMatches(rt.BaseURL, "ai-gateway.vercel.sh") {
		return true
	}
	if p, ok := s.reasoningEndpointProfile(ctx); ok {
		_, declared := s.r.c.o.Providers.ReasoningPayload(ctx, s.scope, p, rt.Model, nil, rt.BaseURL)
		return declared
	}
	if strings.ToLower(strings.TrimSpace(rt.Provider)) == "lmstudio" {
		for _, opt := range s.lmStudioOptions(ctx) {
			if opt != "" && opt != "off" {
				return true
			}
		}
		return false
	}
	if providers.HostMatches(rt.BaseURL, "ollama.com") {
		return s.ollamaSupportsThinking(ctx)
	}
	if !providers.HostMatches(rt.BaseURL, "openrouter.ai") || providers.HostMatches(rt.BaseURL, "api.mistral.ai") {
		return false
	}
	if caps, ok := s.r.c.o.Catalog.OpenRouterReasoningCaps(ctx, rt.Model, false); ok {
		return caps.Supports
	}
	warm := context.WithoutCancel(ctx)
	go s.r.c.o.Catalog.WarmOpenRouterReasoningCaps(warm)
	model := strings.ToLower(rt.Model)
	for _, prefix := range openRouterReasoningPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

func (s *Session) unsetReasoningDefault(ctx context.Context) *wire.Reasoning {
	rt := s.route
	if rt.Mode != wire.ModeChat {
		return nil
	}
	p, ok := s.r.c.o.Providers.Get(ctx, rt.Provider)
	if !ok {
		return nil
	}
	d, ok := p.(providers.ReasoningDefaulter)
	if !ok {
		return nil
	}
	def := d.DefaultReasoning(rt.Model)
	if def == nil {
		return nil
	}
	if caps, known := s.r.c.o.Catalog.Capabilities(ctx, rt.Provider, rt.Model, false, s.r.frozenConfig()); known &&
		caps.SupportsReasoning != nil && !*caps.SupportsReasoning {
		return nil
	}
	if s.o.OllamaNumCtx > 0 && !s.ollamaSupportsThinking(ctx) {
		return nil
	}
	c := *def
	return &c
}

func (s *Session) reasoningForWire(ctx context.Context, ephemeralOff bool) *wire.Reasoning {
	if s.effortRejected {
		s.wireReasoning = nil
		return nil
	}
	cfg := s.o.Reasoning
	if cfg == nil {
		cfg = s.unsetReasoningDefault(ctx)
	} else {
		c := *cfg
		cfg = &c
	}
	if s.disableRejected {
		if cfg != nil && (!cfg.Enabled || cfg.Effort == "none") {
			if s.floorRequired {
				floored := &wire.Reasoning{Enabled: true, Effort: reasoningFloorEffort, Payload: cfg.Payload}
				s.wireReasoning = floored
				return floored
			}
			s.wireReasoning = nil
			return nil
		}
		s.wireReasoning = cfg
		return cfg
	}
	if ephemeralOff {
		off := &wire.Reasoning{Enabled: false, Effort: "none"}
		if cfg != nil {
			off.Payload = cfg.Payload
		}
		cfg = off
	}
	s.wireReasoning = cfg
	return cfg
}

func (s *Session) BeginTurn(history []wire.Message) {
	s.exec.beginTurn()
	s.rec.mu.Lock()
	s.rec.episodes = map[[2]string]*authEpisode{}
	s.rec.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fastUntil = time.Time{}
	if !boundedTierModes[s.o.ServiceTier] {
		return
	}
	if s.o.ServiceTier == "cold" {
		for i := range history {
			switch history[i].Role {
			case wire.RoleUser, wire.RoleAssistant, wire.RoleTool:
				return
			}
		}
	}
	window := defaultFastWindow
	if s.o.FastAutoSeconds != nil {
		window = time.Duration(max(*s.o.FastAutoSeconds, 0) * float64(time.Second))
	}
	s.fastUntil = time.Now().Add(window)
}

func FastModeUnprovisioned(err error, req *wire.Request) bool {
	var apiErr *wire.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests || req == nil || !req.Anthropic.FastMode {
		return false
	}
	if apiErr.Header == nil {
		return false
	}
	for _, name := range fastLimitHeaders {
		if strings.TrimSpace(apiErr.Header.Get(name)) == "0" {
			return true
		}
	}
	return false
}

func (s *Session) MarkFastModeUnavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	model := s.route.Model
	if model == "" || s.fastUnavailable[model] {
		return false
	}
	s.fastUnavailable[model] = true
	return true
}

func (s *Session) anthropicBaseURL() string {
	return anthropic.NormalizeBaseURL(s.route.BaseURL)
}

func (s *Session) effectiveOverrides(ctx context.Context) (map[string]json.RawMessage, error) {
	rt := s.route
	out := map[string]json.RawMessage{}
	for k, v := range s.o.Overrides {
		out[k] = v
	}
	if bounded := boundedTierModes[s.o.ServiceTier]; bounded && time.Now().Before(s.fastUntil) {
		base := rt.BaseURL
		if rt.Mode == wire.ModeAnthropic {
			base = s.anthropicBaseURL()
		}
		if fast, ok := s.r.c.o.Catalog.ResolveFastModeOverrides(ctx, rt.Model, rt.Provider, base, ""); ok {
			if fast.Speed != "" {
				out["speed"] = wire.Quote(fast.Speed)
			}
			if fast.ServiceTier != "" {
				out["service_tier"] = wire.Quote(fast.ServiceTier)
			}
		}
	}
	if _, ok := out["speed"]; ok && s.fastUnavailable[rt.Model] {
		delete(out, "speed")
	}
	if err := s.negotiateTier(ctx, out); err != nil {
		return nil, err
	}
	if err := s.applyDaybreak(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func overrideText(m map[string]json.RawMessage, key string) string {
	v, err := wire.ParseValue(m[key])
	if err != nil {
		return ""
	}
	return v.Text()
}

func (s *Session) codexToken(ctx context.Context) (auth.Secret, error) {
	cred := s.route.Credential
	switch cred.Kind {
	case auth.CredAPIKey, auth.CredXAPIKey:
		return cred.Key, nil
	case auth.CredToken:
		if cred.Tokens == nil {
			return auth.Secret{}, nil
		}
		t, err := cred.Tokens.Token(ctx)
		if err != nil {
			return auth.Secret{}, err
		}
		return t.Value, nil
	}
	return auth.Secret{}, nil
}

func (r *Router) codexCatalog(ctx context.Context, token auth.Secret, baseURL string) ([]wire.Value, bool) {
	entries, fetched, ok := r.c.o.Catalog.CodexCatalogListing(token, baseURL)
	if !ok || time.Since(fetched) >= codexTierTTL {
		r.c.o.Catalog.CodexContextLengths(ctx, token, baseURL)
		entries, fetched, ok = r.c.o.Catalog.CodexCatalogListing(token, baseURL)
	}
	if !ok || time.Since(fetched) >= codexTierTTL {
		return nil, false
	}
	return entries, true
}

func catalogEntry(entries []wire.Value, model string) (wire.Value, bool) {
	slug := catalog.StripCodexContextVariantSuffix(model)
	var found wire.Value
	ok := false
	for _, e := range entries {
		raw, isText := e.Get("slug").Str()
		if !e.IsObject() || !isText {
			continue
		}
		if strings.TrimSpace(raw) == slug {
			found, ok = e, true
		}
	}
	return found, ok
}

func advertisedTiers(entry wire.Value) (map[string]bool, bool) {
	tiers := entry.Get("service_tiers")
	if tiers.IsArray() {
		out := map[string]bool{}
		for _, item := range tiers.Elems() {
			id, ok := item.Get("id").Str()
			if !item.IsObject() || !ok {
				return nil, false
			}
			out[id] = true
		}
		if len(out) > 0 {
			return out, true
		}
	}
	speeds := entry.Get("additional_speed_tiers")
	if speeds.IsArray() {
		out, all := map[string]bool{}, true
		for _, item := range speeds.Elems() {
			word, ok := item.Str()
			if !ok {
				all = false
				break
			}
			if word == "fast" {
				word = "priority"
			}
			out[word] = true
		}
		if all {
			return out, true
		}
	}
	if tiers.IsArray() && len(tiers.Elems()) == 0 {
		return map[string]bool{}, true
	}
	return nil, false
}

func cyberPrograms(entry wire.Value) (map[string]bool, bool) {
	cyber := entry.Path("available_access_programs", "cyber")
	if !entry.Get("available_access_programs").IsObject() || !cyber.IsArray() {
		return nil, false
	}
	out := map[string]bool{}
	for _, item := range cyber.Elems() {
		word, ok := item.Str()
		if !ok {
			return nil, false
		}
		out[word] = true
	}
	return out, true
}

func (s *Session) officialCodexRoute() bool {
	rt := s.route
	return rt.Mode == wire.ModeResponses && rt.Provider == "openai-codex" && openai.IsOfficialCodexURL(rt.BaseURL)
}

func (s *Session) negotiateTier(ctx context.Context, overrides map[string]json.RawMessage) error {
	requested := overrideText(overrides, "service_tier")
	if (requested != "ultrafast" && requested != "priority") || !s.officialCodexRoute() {
		return nil
	}
	token, err := s.codexToken(ctx)
	if err != nil || token.Empty() {
		return err
	}
	model := s.route.Model
	if m := overrideText(overrides, "model"); m != "" {
		model = m
	}
	entries, ok := s.r.codexCatalog(ctx, token, s.route.BaseURL)
	if !ok {
		return nil
	}
	entry, ok := catalogEntry(entries, model)
	if !ok {
		return nil
	}
	supported, ok := advertisedTiers(entry)
	if !ok {
		return nil
	}
	candidates := []string{"priority"}
	if requested == "ultrafast" {
		candidates = []string{"ultrafast", "priority"}
	}
	effective := "default"
	for _, tier := range candidates {
		if supported[tier] {
			effective = tier
			break
		}
	}
	if effective == "default" {
		delete(overrides, "service_tier")
	} else {
		overrides["service_tier"] = wire.Quote(effective)
	}
	if effective != requested {
		slog.Debug("Codex service tier not advertised for the selected credential", "requested", requested, "effective", effective)
	}
	return nil
}

func (s *Session) applyDaybreak(ctx context.Context, overrides map[string]json.RawMessage) error {
	if !s.o.Daybreak || !s.officialCodexRoute() {
		return nil
	}
	token, err := s.codexToken(ctx)
	if err != nil || token.Empty() {
		return err
	}
	model := s.route.Model
	if m := overrideText(overrides, "model"); m != "" {
		model = m
	}
	entries, ok := s.r.codexCatalog(ctx, token, s.route.BaseURL)
	if !ok {
		return nil
	}
	entry, ok := catalogEntry(entries, model)
	if !ok {
		return nil
	}
	programs, ok := cyberPrograms(entry)
	if !ok || !programs[daybreakProgram] {
		return nil
	}
	extra := &wire.Dict{}
	if v, err := wire.ParseValue(overrides["extra_body"]); err == nil && v.IsObject() {
		extra = wire.DictOf(v)
	}
	programsDict := &wire.Dict{}
	if v := extra.Get("access_programs"); v.IsObject() {
		programsDict = wire.DictOf(v)
	}
	programsDict.SetString("cyber", daybreakProgram)
	extra.Set("access_programs", programsDict.JSON())
	overrides["extra_body"] = extra.JSON()
	return nil
}

func ClassifyPortalUpstream(resp *wire.Response) string {
	if resp == nil {
		return ""
	}
	if resp.Upstream != "" {
		return "openrouter"
	}
	switch {
	case openRouterGenerationID.MatchString(resp.ID):
		return "openrouter"
	case strings.HasPrefix(resp.ID, "msg_"):
		return "gmi"
	}
	return ""
}

func portalWireFor(upstream string) wire.Mode {
	if upstream == "gmi" && gmiNativeWireCleared {
		return wire.ModeAnthropic
	}
	return wire.ModeChat
}

func (s *Session) NotePortalResponse(ctx context.Context, resp *wire.Response, callCount int) (bool, error) {
	ctx = s.r.bind(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	rt := s.route
	if callCount != 1 || s.portalDecided || strings.ToLower(rt.Provider) != "portal" ||
		!strings.HasPrefix(strings.ToLower(rt.Model), "anthropic/") {
		return false, nil
	}
	cfg := s.r.frozenConfig()
	if cfg == nil {
		live, err := s.r.c.o.Config.LoadReadonly(ctx)
		if err != nil {
			return false, err
		}
		cfg = live
	}
	if providers.PortalAnthropicWire(cfg) != config.WireAuto {
		return false, nil
	}
	s.portalDecided = true
	upstream := ClassifyPortalUpstream(resp)
	want := portalWireFor(upstream)
	if want == rt.Mode {
		slog.Debug("portal wire auto keeps the current wire", "upstream", upstream, "mode", string(want))
		return false, nil
	}
	s.portalPending, s.portalUpstream = want, upstream
	return true, nil
}

func (s *Session) applyPendingWire() {
	if s.portalPending == "" {
		return
	}
	want := s.portalPending
	s.portalPending = ""
	next := *s.route
	next.Mode = want
	s.route = &next
	slog.Info("portal wire auto switched the session wire", "upstream", s.portalUpstream, "mode", string(want), "session", s.sessionID)
}

func thinkingFingerprint(block wire.Value) (string, bool) {
	if !block.IsObject() {
		return "", false
	}
	kind := block.Get("type").Text()
	field := "data"
	switch kind {
	case "thinking":
		field = "signature"
	case "redacted_thinking":
	default:
		return "", false
	}
	v := block.Get(field)
	payload, isText := v.Str()
	if v.Kind() <= wire.KindNull || (isText && payload == "") {
		return "", false
	}
	if !isText {
		payload = string(v.Raw())
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + payload))
	return hex.EncodeToString(sum[:]), true
}

func (s *Session) tracksRejectedThinking() bool {
	rt := s.route
	if rt.Mode != wire.ModeAnthropic {
		return false
	}
	return anthropic.RouteSignsThinking(anthropic.ThinkingRoute(rt.BaseURL, rt.Model, portalRoute(rt)))
}

func portalRoute(rt *Route) bool {
	return strings.ToLower(rt.Provider) == "portal" || providers.HostMatches(rt.BaseURL, "icebath.app")
}

func anthropicPreservesPriorThinking(baseURL, model string) bool {
	return anthropic.PreservesPriorThinking(baseURL, model, providers.HostMatches(baseURL, "icebath.app"))
}

func (s *Session) persistedThinking(ctx context.Context, sessionID string) map[string]bool {
	out := map[string]bool{}
	if sessionID == "" || s.o.Store == nil {
		return out
	}
	raw, ok, err := s.o.Store.ModelConfigValue(ctx, sessionID, rejectedThinkingKey)
	if err != nil {
		slog.Debug("Anthropic thinking suppression restore failed", "error", err)
		return out
	}
	if !ok {
		return out
	}
	if v, perr := wire.ParseValue(raw); perr == nil && v.IsArray() {
		for _, e := range v.Elems() {
			if str, isText := e.Str(); isText && str != "" {
				out[str] = true
			}
		}
	}
	return out
}

func (s *Session) rejectedThinkingLocked(ctx context.Context) map[string]bool {
	if !s.thinkingBound || s.thinkingFor != s.sessionID {
		s.thinking = s.persistedThinking(ctx, s.sessionID)
		s.thinkingFor, s.thinkingBound = s.sessionID, true
	}
	return s.thinking
}

func (s *Session) persistThinking(ctx context.Context) error {
	if s.o.PersistDisabled || s.o.Store == nil || s.sessionID == "" {
		return nil
	}
	list := make([]string, 0, len(s.thinking))
	for k := range s.thinking {
		list = append(list, k)
	}
	sort.Strings(list)
	return s.o.Store.PatchModelConfig(ctx, s.sessionID, map[string]json.RawMessage{rejectedThinkingKey: wire.Strings(list)})
}

func (s *Session) mirroredThinking(m *wire.Message) (string, bool) {
	_, texts := s.r.c.estimator.replayCarrier(m.Members())
	if len(texts) == 0 {
		return "", false
	}
	return strings.Join(texts, "\n\n"), true
}

func stripThinkingBlocks(blocks []json.RawMessage, rejected map[string]bool) ([]json.RawMessage, int) {
	if len(blocks) == 0 {
		return blocks, 0
	}
	kept := make([]json.RawMessage, 0, len(blocks))
	removed := 0
	for _, raw := range blocks {
		v, err := wire.ParseValue(raw)
		kind := ""
		if err == nil && v.IsObject() {
			kind = v.Get("type").Text()
		}
		if kind == "thinking" || kind == "redacted_thinking" {
			fp, ok := thinkingFingerprint(v)
			if rejected == nil || (ok && rejected[fp]) {
				removed++
				continue
			}
		}
		kept = append(kept, raw)
	}
	if len(kept) == 0 {
		kept = nil
	}
	return kept, removed
}

func (s *Session) stripRejectedThinking(m *wire.Message, rejected map[string]bool) int {
	mirror, hasMirror := "", false
	if m.Role == wire.RoleAssistant {
		mirror, hasMirror = s.mirroredThinking(m)
	}
	var a, b int
	m.ReasoningDetails, a = stripThinkingBlocks(m.ReasoningDetails, rejected)
	m.AnthropicBlocks, b = stripThinkingBlocks(m.AnthropicBlocks, rejected)
	removed := a + b
	if removed > 0 && hasMirror {
		if m.Reasoning == mirror {
			m.Reasoning = ""
		}
		if m.ReasoningContent != nil && *m.ReasoningContent == mirror {
			m.ReasoningContent = nil
		}
	}
	return removed
}

func (s *Session) suppressRejectedThinking(ctx context.Context, msgs []wire.Message) {
	if !s.tracksRejectedThinking() {
		return
	}
	rejected := s.rejectedThinkingLocked(ctx)
	if len(rejected) == 0 {
		return
	}
	for i := range msgs {
		s.stripRejectedThinking(&msgs[i], rejected)
	}
}

func (s *Session) RememberRejectedThinking(ctx context.Context, sent []wire.Message) ([]wire.Message, int, error) {
	ctx = s.r.bind(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	out := cloneMessages(sent)
	current := map[string]bool{}
	for i := range out {
		for _, raw := range append(append([]json.RawMessage{}, out[i].ReasoningDetails...), out[i].AnthropicBlocks...) {
			if v, err := wire.ParseValue(raw); err == nil {
				if fp, ok := thinkingFingerprint(v); ok {
					current[fp] = true
				}
			}
		}
	}
	removed := 0
	if len(current) == 0 {
		for i := range out {
			removed += s.stripRejectedThinking(&out[i], nil)
		}
		return out, removed, nil
	}
	rejected := s.rejectedThinkingLocked(ctx)
	for fp := range current {
		rejected[fp] = true
	}
	err := s.persistThinking(ctx)
	if err != nil {
		slog.Debug("Anthropic thinking suppression persist failed", "error", err)
	}
	for i := range out {
		removed += s.stripRejectedThinking(&out[i], rejected)
	}
	return out, removed, nil
}

func (s *Session) CarryRejectedThinking(ctx context.Context, oldSessionID, newSessionID string) error {
	ctx = s.r.bind(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	carried := map[string]bool{}
	if s.thinkingBound && s.thinkingFor == oldSessionID {
		for k := range s.thinking {
			carried[k] = true
		}
	} else {
		carried = s.persistedThinking(ctx, oldSessionID)
	}
	s.sessionID = newSessionID
	if len(carried) == 0 {
		return nil
	}
	for k := range s.rejectedThinkingLocked(ctx) {
		carried[k] = true
	}
	s.thinking, s.thinkingFor, s.thinkingBound = carried, newSessionID, true
	return s.persistThinking(ctx)
}

func slotKeyedCacheRoute(provider, model, baseURL string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "xai" || p == "xai-oauth" || providers.URLHostname(baseURL) == "api.x.ai" {
		return true
	}
	m := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range grokAggregatorPrefixes {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

func (s *Session) applyForkTag(scope string, turn TurnFacts) string {
	if scope == "" || s.o.ForkTag == "" || turn.CompressionCount < 1 {
		return scope
	}
	if !slotKeyedCacheRoute(s.route.Provider, s.route.Model, s.route.BaseURL) {
		return scope
	}
	return scope + ForkScopeSeparator + s.o.ForkTag
}

func (s *Session) declaredScope(ctx context.Context) (string, bool) {
	if strings.HasPrefix(s.o.InheritedCacheScope, declaredScopePrefix) {
		return s.o.InheritedCacheScope, true
	}
	key := strings.TrimSpace(s.o.GatewaySessionKey)
	if key == "" || s.o.PersistDisabled {
		return "", false
	}
	sid, db := s.sessionID, s.o.Store
	source := ""
	if sid != "" && db != nil {
		fork, rowSource, err := db.ScopeIdentity(ctx, sid)
		if err != nil {
			slog.Debug("declared-scope fork check failed", "error", err)
			return "", false
		}
		if fork {
			return "", false
		}
		source = strings.TrimSpace(rowSource)
	}
	if source == "" {
		source = strings.TrimSpace(s.o.Source)
	}
	generation := ""
	if db != nil {
		gen, ok, err := db.ConversationGeneration(ctx, key, source)
		if err != nil {
			slog.Debug("declared-scope generation read failed", "error", err)
			return "", false
		}
		if ok {
			generation = strconv.FormatInt(gen, 10)
		}
	}
	sum := sha256.Sum256([]byte(source + "|" + key + "|" + generation))
	return declaredScopePrefix + hex.EncodeToString(sum[:])[:24], true
}

func (s *Session) cacheScope(ctx context.Context, turn TurnFacts) string {
	if s.o.InheritedCacheScope != "" {
		return s.applyForkTag(s.o.InheritedCacheScope, turn)
	}
	sid := s.sessionID
	if sid == "" {
		return ""
	}
	db := s.o.Store != nil
	if s.scopeMemoSet && s.scopeMemoSID == sid && s.scopeMemoDB == db {
		return s.applyForkTag(s.scopeMemo, turn)
	}
	root, found := s.declaredScope(ctx)
	if !found && db {
		lineage, err := s.o.Store.CompressionLineage(ctx, sid)
		if err != nil {
			slog.Debug("prompt-cache scope lineage walk failed", "error", err)
		} else if len(lineage) > 0 && lineage[0] != "" {
			root, found = lineage[0], true
		}
	}
	scope := sid
	if found {
		scope = root
	}
	if found || !db || s.o.PersistDisabled {
		s.scopeMemoSet, s.scopeMemoSID, s.scopeMemoDB, s.scopeMemo = true, sid, db, scope
	}
	return s.applyForkTag(scope, turn)
}

func NativeCompactionModel(model, provider, baseURL string) bool {
	return strings.Contains(strings.ToLower(model), "gpt-5.6") ||
		(wire.IsAstraModel(model) && strings.ToLower(strings.TrimSpace(provider)) == "openai-codex" && openai.IsOfficialCodexURL(baseURL))
}

func DirectOpenAIRoute(baseURL string, codexBackend bool) bool {
	return codexBackend || providers.URLHostname(baseURL) == "api.openai.com"
}

func NativeCompactionCapable(model, baseURL, provider string, codexBackend bool) bool {
	direct := strings.ToLower(strings.TrimSpace(provider)) == "openai" && baseURL == ""
	return NativeCompactionModel(model, provider, baseURL) && (direct || DirectOpenAIRoute(baseURL, codexBackend))
}

func CompactThreshold(configured, localTrigger int) int {
	upper, hasUpper := 0, localTrigger > 0
	if hasUpper {
		if localTrigger > localTriggerSafetyMargin {
			upper = localTrigger - localTriggerSafetyMargin
		} else {
			upper = int(float64(localTrigger) * 0.8)
		}
		upper = max(1024, upper)
	}
	if configured <= 0 {
		if hasUpper {
			return upper
		}
		return defaultCompactThreshold
	}
	if !hasUpper {
		return configured
	}
	return max(1024, min(configured, upper))
}

func (s *Session) contextManagement(turn TurnFacts, codexBackend, xai, endpointNative bool) json.RawMessage {
	rt := s.route
	if !NativeCompactionCapable(rt.Model, rt.BaseURL, rt.Provider, codexBackend) {
		return nil
	}
	if !s.o.NativeCompaction || s.o.CompressionDisabled {
		return nil
	}
	if s.o.CheckpointRequired {
		s.r.c.warnOnce("checkpoint-compaction", "compression.checkpoint_required is enabled: server-side native compaction (context_management) is disabled for this agent so the checkpoint-aware icebath compressor stays authoritative.")
		return nil
	}
	if xai || !endpointNative || !NativeCompactionModel(rt.Model, rt.Provider, rt.BaseURL) {
		return nil
	}
	if !rt.Capabilities["openai_native_compaction"] && !DirectOpenAIRoute(rt.BaseURL, codexBackend) {
		return nil
	}
	threshold := CompactThreshold(s.o.CompactThreshold, turn.LocalTriggerTokens)
	item := (&wire.Dict{}).SetString("type", "compaction").SetInt("compact_threshold", threshold)
	return wire.Array(item.JSON())
}
