package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type recovery struct {
	mu              sync.Mutex
	primary         *RouteState
	chain           []config.FallbackEntry
	index           int
	activated       bool
	providerActive  bool
	fbModel         string
	fbProvider      string
	hasFBRoute      bool
	unavailable     map[[3]string]bool
	entitlement     map[[2]string]bool
	limitedUntil    time.Time
	backoffs        int
	waitLogged      bool
	reopenCheckedAt time.Time
	entryID         string
	revertID        string
	episodes        map[[2]string]*authEpisode
	envSeen         *envSeen
}

type envSeen struct {
	baseURL string
	key     auth.Secret
}

type RouteState struct {
	route          *Route
	overrides      map[string]json.RawMessage
	echo           bool
	reasoning      *wire.Reasoning
	reasoningSet   bool
	owner          *auth.CustomEntry
	activated      bool
	index          int
	providerActive bool
	fbModel        string
	fbProvider     string
	hasFBRoute     bool
	backoffs       int
	entryID        string
	effortRejected bool
	disableRejectd bool
	floorRequired  bool
	withRecovery   bool
	chain          []config.FallbackEntry
	primary        *RouteState
	revertID       string
}

func (st *RouteState) Route() *Route {
	if st == nil {
		return nil
	}
	return st.route
}

func cloneRoute(rt *Route) *Route {
	if rt == nil {
		return nil
	}
	c := *rt
	c.Args = slices.Clone(rt.Args)
	c.Capabilities = maps.Clone(rt.Capabilities)
	c.Headers = maps.Clone(rt.Headers)
	c.ExtraBody = maps.Clone(rt.ExtraBody)
	return &c
}

func cloneReasoning(r *wire.Reasoning) *wire.Reasoning {
	if r == nil {
		return nil
	}
	c := *r
	c.Payload = append(json.RawMessage(nil), r.Payload...)
	return &c
}

func cloneOverrides(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func (s *Session) initRecovery(ctx context.Context) {
	s.rec.chain = slices.Clone(s.o.Fallback)
	s.rec.unavailable = map[[3]string]bool{}
	s.rec.entitlement = map[[2]string]bool{}
	s.rec.episodes = map[[2]string]*authEpisode{}
	s.rec.entryID = poolEntryIDFor(s.route)
	s.rec.primary = s.captureLocked(ctx, false, false)
}

func poolEntryIDFor(rt *Route) string {
	if rt == nil || rt.Pool == nil {
		return ""
	}
	id, ok := rt.Pool.EntryIDForAPIKey(rt.Credential.Key)
	if !ok {
		return ""
	}
	return id
}

func (s *Session) captureLocked(ctx context.Context, withReasoning, withRecovery bool) *RouteState {
	rt := s.route
	st := &RouteState{
		route: cloneRoute(rt), overrides: cloneOverrides(s.o.Overrides), echo: s.echoOptIn,
		activated: s.rec.activated, index: s.rec.index, providerActive: s.rec.providerActive,
		fbModel: s.rec.fbModel, fbProvider: s.rec.fbProvider, hasFBRoute: s.rec.hasFBRoute,
		backoffs: s.rec.backoffs, entryID: s.rec.entryID,
		effortRejected: s.effortRejected, disableRejectd: s.disableRejected, floorRequired: s.floorRequired,
	}
	if withReasoning {
		st.reasoning, st.reasoningSet = cloneReasoning(s.o.Reasoning), true
	}
	if withRecovery {
		st.withRecovery, st.chain, st.primary, st.revertID = true, slices.Clone(s.rec.chain), s.rec.primary, s.rec.revertID
	}
	if rt != nil {
		if owner, ok := s.r.c.o.Auth.RuntimeEntryOwner(ctx, rt.Provider, rt.Requested, rt.BaseURL); ok {
			st.owner = &owner
		}
	}
	return st
}

func (s *Session) Capture(ctx context.Context) *RouteState {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.captureLocked(ctx, true, true)
}

func (s *Session) Reinstall(ctx context.Context, st *RouteState) error {
	if st == nil || st.route == nil {
		return fmt.Errorf("model: no route state to reinstall")
	}
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.reinstallRuntime(ctx, st)
	s.mu.Lock()
	s.route.Pool = st.route.Pool
	s.rec.activated, s.rec.index, s.rec.providerActive = st.activated, st.index, st.providerActive
	s.rec.fbModel, s.rec.fbProvider, s.rec.hasFBRoute = st.fbModel, st.fbProvider, st.hasFBRoute
	s.rec.backoffs, s.rec.entryID = st.backoffs, st.entryID
	s.effortRejected, s.disableRejected, s.floorRequired = st.effortRejected, st.disableRejectd, st.floorRequired
	if st.withRecovery {
		s.rec.chain, s.rec.primary, s.rec.revertID = slices.Clone(st.chain), st.primary, st.revertID
	}
	s.mu.Unlock()
	return nil
}

func (s *Session) reinstallRuntime(ctx context.Context, st *RouteState) {
	rt := cloneRoute(st.route)
	if s.r.c.o.Providers.IsActualRoute(ctx, rt.Provider, rt.BaseURL) {
		rt.Mode = wire.ModeChat
	}
	s.mu.Lock()
	if cur := s.route; cur != nil && cur.Pool != nil {
		rt.Pool = cur.Pool
	}
	s.route = rt
	s.echoOptIn = st.echo
	s.o.Overrides = cloneOverrides(st.overrides)
	s.o.NoEncryptedReplay = false
	if st.reasoningSet {
		s.o.Reasoning = cloneReasoning(st.reasoning)
	}
	s.mu.Unlock()
	s.exec.resetStreak()
}

func (s *Session) Reasoning() *wire.Reasoning {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneReasoning(s.o.Reasoning)
}

func (s *Session) SetReasoning(r *wire.Reasoning) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.o.Reasoning = cloneReasoning(r)
}

func (s *Session) Overrides() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneOverrides(s.o.Overrides)
}

func (s *Session) SetOverrides(m map[string]json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.o.Overrides = cloneOverrides(m)
}

func (s *Session) DisableEncryptedReplay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.o.NoEncryptedReplay = true
}

func (s *Session) FallbackActive() bool {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.rec.activated
}

func (s *Session) HasPendingFallback() bool {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.rec.index < len(s.rec.chain)
}

func (s *Session) FallbackChain() []config.FallbackEntry {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return slices.Clone(s.rec.chain)
}

func (s *Session) PoolEntryID() string {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.rec.entryID
}

func (s *Session) PoolMayRecover(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	pool := s.Route().Pool
	if pool == nil {
		return false, nil
	}
	ok, err := pool.HasAvailable(ctx, "")
	if err != nil || !ok {
		return false, err
	}
	return len(pool.Entries()) > 1, nil
}

func (s *Session) setRoute(rt *Route) {
	s.mu.Lock()
	s.route = rt
	s.mu.Unlock()
}

func (s *Session) syncEntryID() {
	s.rec.entryID = poolEntryIDFor(s.Route())
}

func routeProbeKey(rt *Route) local.ProbeKey {
	cred := rt.Credential
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

func (s *Session) ensureLMStudioLoaded(ctx context.Context, rt *Route, contextLength *int) (local.LMStudioLoadResult, bool, error) {
	if strings.ToLower(strings.TrimSpace(rt.Provider)) != "lmstudio" {
		return local.LMStudioLoadResult{}, false, nil
	}
	cfg, err := s.liveConfig(ctx)
	if err != nil {
		return local.LMStudioLoadResult{}, false, err
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Model.LMStudioLoadMode))
	if mode == "jit" {
		slog.Debug("LM Studio explicit preload skipped: lmstudio_load_mode=jit")
		return local.LMStudioLoadResult{}, false, nil
	}
	return s.r.c.o.Local.EnsureLMStudioModelLoaded(ctx, rt.Model, rt.BaseURL, routeProbeKey(rt), contextLength, 0), true, nil
}

func (s *Session) liveConfig(ctx context.Context) (*config.Config, error) {
	if cfg := s.r.frozenConfig(); cfg != nil {
		return cfg, nil
	}
	return s.r.c.o.Config.LoadReadonly(ctx)
}

func (s *Session) customEndpoints(ctx context.Context) ([]config.CustomEndpoint, error) {
	if s.r.frozen != nil {
		return s.r.frozen.custom, nil
	}
	return s.r.c.o.Config.CustomEndpoints(ctx)
}

func (s *Session) routeContextLength(ctx context.Context, rt *Route, configured int) (int, bool, error) {
	custom, err := s.customEndpoints(ctx)
	if err != nil {
		return 0, false, err
	}
	n, known := s.r.c.o.Catalog.ContextLength(ctx, catalog.ContextQuery{Model: rt.Model, BaseURL: rt.BaseURL, Key: rt.Credential,
		Configured: configured, Provider: rt.Provider, Custom: custom})
	return n, known, nil
}

func ResolveReasoningConfig(cfg *config.Config, model string) *wire.Reasoning {
	if cfg == nil {
		return nil
	}
	if model == "" {
		model = strings.TrimSpace(cfg.Model.Default.ID)
	}
	if r, ok := perModelReasoning(model, cfg.Agent.ReasoningOverrides); ok {
		return r
	}
	re := cfg.Agent.ReasoningEffort
	if re == nil {
		return nil
	}
	enabled, level, set := re.Parse()
	if !set {
		raw := strings.TrimSpace(re.Level.String())
		switch b, isBool := re.Level.AsBool(); {
		case re.Mapping:
			raw = fmt.Sprintf("{'enabled': %s, 'effort': %s}", re.Enabled.String(), re.Effort.String())
		case isBool && b:
			raw = "True"
		}
		if raw != "" {
			slog.Warn(fmt.Sprintf("Unknown reasoning_effort '%s', using default (medium)", raw))
		}
		return nil
	}
	return &wire.Reasoning{Enabled: enabled, Effort: level}
}

func perModelReasoning(model string, overrides config.Map[config.ReasoningEffort]) (*wire.Reasoning, bool) {
	if model == "" || overrides.Len() == 0 {
		return nil, false
	}
	parse := func(key string) (*wire.Reasoning, bool) {
		v, ok := overrides.Get(key)
		if !ok {
			return nil, false
		}
		enabled, level, set := v.Parse()
		if !set {
			return nil, false
		}
		return &wire.Reasoning{Enabled: enabled, Effort: level}, true
	}
	variants := modelVariants(model)
	for _, v := range variants {
		if r, ok := parse(v); ok {
			return r, true
		}
	}
	set := map[string]bool{}
	for _, v := range variants {
		set[v] = true
	}
	for _, key := range overrides.Keys() {
		if !strings.Contains(key, "/") {
			continue
		}
		parts := strings.Split(key, "/")
		forms := modelVariants(parts[len(parts)-1])
		if len(parts) >= 3 {
			forms = append(forms, modelVariants(strings.Join(parts[1:], "/"))...)
		}
		if slices.ContainsFunc(forms, func(f string) bool { return set[f] }) {
			if r, ok := parse(key); ok {
				return r, true
			}
		}
	}
	return nil, false
}
