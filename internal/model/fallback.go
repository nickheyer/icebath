package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	fallbackExhaustedCooldown = 5 * time.Second
	maxRateLimitCooldown      = 14400
	poolSkipHorizon           = 600 * time.Second
)

type FailureScope uint8

const (
	ScopeModel FailureScope = iota
	ScopeCredential
	ScopeEndpoint
)

type BackendIdentity struct {
	Provider string
	Model    string
	BaseURL  string
}

func NewBackendIdentity(provider, model, baseURL string) BackendIdentity {
	norm := func(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
	return BackendIdentity{Provider: norm(provider), Model: norm(model), BaseURL: catalog.NormalizeRouteBaseURL(baseURL)}
}

func (r *Router) bothFirstClass(ctx context.Context, a, b BackendIdentity) bool {
	if a.Provider == "" || b.Provider == "" || a.Provider == b.Provider {
		return false
	}
	reg := r.c.o.Providers
	_, aok := reg.RegistryRow(ctx, a.Provider)
	_, bok := reg.RegistryRow(ctx, b.Provider)
	return aok && bok
}

func sameCredentialSurface(a, b BackendIdentity) bool {
	if a.Provider != "" && b.Provider != "" {
		return a.Provider == b.Provider
	}
	return a.BaseURL != "" && a.BaseURL == b.BaseURL
}

func sameEndpoint(a, b BackendIdentity) bool {
	if a.BaseURL != "" && b.BaseURL != "" {
		return a.BaseURL == b.BaseURL
	}
	return a.Provider != "" && a.Provider == b.Provider
}

func (r *Router) sameDeployment(ctx context.Context, a, b BackendIdentity) bool {
	if !(a.Provider != "" && b.Provider != "" && a.Provider == b.Provider) {
		return a.BaseURL != "" && a.BaseURL == b.BaseURL && a.Model != "" && a.Model == b.Model && !r.bothFirstClass(ctx, a, b)
	}
	if !(a.Model != "" && b.Model != "" && a.Model == b.Model) {
		return false
	}
	return !(a.BaseURL != "" && b.BaseURL != "" && a.BaseURL != b.BaseURL)
}

func (r *Router) ShouldSkipCandidate(ctx context.Context, candidate, failed BackendIdentity, scope FailureScope) bool {
	ctx = r.bind(ctx)
	switch scope {
	case ScopeCredential:
		return sameCredentialSurface(candidate, failed)
	case ScopeEndpoint:
		return sameEndpoint(candidate, failed)
	}
	return r.sameDeployment(ctx, candidate, failed)
}

var fallbackReasonLabels = map[Class]string{
	ClassAuth:                    "authentication failed",
	ClassAuthPermanent:           "authentication permanently failed",
	ClassBilling:                 "billing or quota exhausted",
	ClassRateLimit:               "rate limit",
	ClassUpstreamRateLimit:       "upstream model rate limit",
	ClassOverloaded:              "provider overloaded",
	ClassServerError:             "provider server error",
	ClassTimeout:                 "request timeout",
	ClassSSLCertVerification:     "TLS certificate verification failed",
	ClassContextOverflow:         "context window exceeded",
	ClassPayloadTooLarge:         "request payload too large",
	ClassImageTooLarge:           "image payload too large",
	ClassModelNotFound:           "model not found",
	ClassProviderPolicyBlocked:   "provider policy blocked the request",
	ClassContentPolicyBlocked:    "content policy blocked the request",
	ClassFormatError:             "request format rejected",
	ClassRoleAlternation:         "adjacent same-role messages rejected",
	ClassInvalidEncryptedContent: "encrypted reasoning state rejected",
	ClassMultimodalToolContent:   "multimodal tool content unsupported",
	ClassThinkingSignature:       "thinking signature rejected",
	ClassLongContextTier:         "long-context tier unavailable",
	ClassOAuthLongContextBeta:    "OAuth long-context beta unavailable",
	ClassGrammarPattern:          "grammar pattern rejected",
	ClassUnknown:                 "provider failure",
}

func FallbackReasonText(reason Class) string {
	if label, ok := fallbackReasonLabels[reason]; ok {
		return label
	}
	return strings.ReplaceAll(reason.String(), "_", " ")
}

func rateLimitFailover(reason Class) bool {
	return reason == ClassRateLimit || reason == ClassBilling || reason == ClassUpstreamRateLimit
}

func providerResetDelay(resetAt time.Time) (time.Duration, bool) {
	if resetAt.IsZero() {
		return 0, false
	}
	d := time.Until(resetAt)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

func (s *Session) switchDeferredByReset(ctx context.Context, reason Class, resetAt time.Time) bool {
	if !rateLimitFailover(reason) || s.rec.activated {
		return false
	}
	cfg, err := s.r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		return false
	}
	threshold := cfg.Fallback.MinSwitchResetSeconds
	if !(threshold > 0) {
		return false
	}
	delay, ok := providerResetDelay(resetAt)
	if !ok || delay.Seconds() >= threshold {
		return false
	}
	slog.Info(fmt.Sprintf("Rate limit resets in %.0f s (< fallback.min_switch_reset_seconds=%.0f): staying on the primary", delay.Seconds(), threshold))
	return true
}

func (s *Session) armRateLimitCooldown(reason Class, resetAt time.Time) (int, bool) {
	if !rateLimitFailover(reason) {
		return 0, false
	}
	current := strings.ToLower(strings.TrimSpace(s.Route().Provider))
	primary := ""
	if p := s.rec.primary.Route(); p != nil {
		primary = strings.ToLower(strings.TrimSpace(p.Provider))
	}
	if s.rec.activated && !(primary != "" && current == primary) {
		return 0, false
	}
	count := s.rec.backoffs
	s.rec.backoffs++
	var secs int
	source := "exponential fallback"
	if delay, ok := providerResetDelay(resetAt); ok {
		secs, source = int(math.Ceil(delay.Seconds())), "provider reset"
	} else {
		secs = maxRateLimitCooldown
		if count < 8 {
			secs = min(60<<count, maxRateLimitCooldown)
		}
	}
	s.rec.limitedUntil = time.Now().Add(time.Duration(secs) * time.Second)
	slog.Info(fmt.Sprintf("Rate-limit backoff level %d: cooldown %d s (%.1f min, backoff#%d, %s)", count, secs, float64(secs)/60, count+1, source))
	return secs, true
}

func (s *Session) MarkEntitlementRejected(ctx context.Context, e *Error) (notice string, marked bool, err error) {
	if e == nil || e.Status != 400 {
		return "", false, nil
	}
	ctx = s.r.bind(ctx)
	haystack := strings.ToLower(e.Message)
	if e.Err != nil {
		haystack += " " + strings.ToLower(e.Err.Error())
	}
	if !strings.Contains(haystack, CodexEntitlementMarker) {
		return "", false, nil
	}
	rt := s.Route()
	provider, model := strings.ToLower(strings.TrimSpace(rt.Provider)), strings.TrimSpace(rt.Model)
	if provider == "" || model == "" {
		return "", false, nil
	}
	if rt.Pool != nil {
		avail, aerr := rt.Pool.HasAvailable(ctx, model)
		if aerr != nil {
			return "", false, aerr
		}
		if avail {
			return "", false, nil
		}
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.rec.entitlement[[2]string{provider, model}] {
		return "", true, nil
	}
	s.rec.entitlement[[2]string{provider, model}] = true
	slog.Warn(fmt.Sprintf("Model entitlement rejection: this account is not entitled to %s via %s; treating it as unavailable for this session", model, provider))
	return fmt.Sprintf("🚫 This account is not entitled to %s via %s; it will be skipped until restart. Switch to an entitled model via /model or `icebath model`.", model, provider), true, nil
}

func (s *Session) entitlementRejected(ctx context.Context, provider, model string) bool {
	if len(s.rec.entitlement) == 0 {
		return false
	}
	if s.rec.entitlement[[2]string{provider, model}] {
		return true
	}
	return s.rec.entitlement[[2]string{provider, s.r.c.o.Catalog.NormalizeModelForProvider(ctx, model, provider)}]
}

func fallbackEntryKey(fb config.FallbackEntry) [3]string {
	return [3]string{strings.ToLower(strings.TrimSpace(fb.Provider)), strings.TrimSpace(fb.Model),
		strings.TrimRight(strings.TrimSpace(fb.BaseURL), "/")}
}

func (s *Session) entryUnavailableWithoutNetwork(ctx context.Context, fb config.FallbackEntry) string {
	if strings.ToLower(strings.TrimSpace(fb.Provider)) != portal.Provider {
		return ""
	}
	state, _, err := s.r.c.o.Auth.State(ctx, portal.Provider)
	if err != nil {
		return "portal_auth_unreadable:" + errorTypeName(err)
	}
	for _, key := range []string{"access_token", "refresh_token"} {
		if strings.TrimSpace(stateText(state, key)) != "" {
			return ""
		}
	}
	return "portal_token_missing"
}

func (s *Session) candidatePoolExhausted(ctx context.Context, provider, model string) (bool, error) {
	pool := s.Route().Pool
	if pool == nil || strings.ToLower(strings.TrimSpace(pool.Provider())) != provider {
		loaded, err := s.r.c.o.Auth.Pool(ctx, provider)
		if err != nil {
			return false, nil
		}
		pool = loaded
	}
	if pool == nil || !pool.HasCredentials() {
		return false, nil
	}
	avail, err := pool.HasAvailable(ctx, model)
	if err != nil || avail {
		return false, err
	}
	until, ok, err := pool.NextAvailableAt(ctx, model)
	if err != nil {
		return false, err
	}
	return !ok || time.Until(until) > poolSkipHorizon, nil
}

func (s *Session) skipFallbackCandidate(ctx context.Context, fb config.FallbackEntry, key [3]string, provider, model string) (bool, error) {
	if s.rec.unavailable[key] {
		slog.Debug(fmt.Sprintf("Fallback skip: %v previously marked unavailable", key))
		return true, nil
	}
	if provider == "" || model == "" {
		return true, nil
	}
	if s.entitlementRejected(ctx, provider, model) {
		slog.Info(fmt.Sprintf("Fallback skip: %s/%s was rejected as unentitled for this account", provider, model))
		return true, nil
	}
	exhausted, err := s.candidatePoolExhausted(ctx, provider, model)
	if err != nil {
		return false, err
	}
	if exhausted {
		slog.Warn(fmt.Sprintf("Fallback skip: %s/%s credential pool is exhausted (every entry in cooldown)", provider, model))
		return true, nil
	}
	if reason := s.entryUnavailableWithoutNetwork(ctx, fb); reason != "" {
		s.rec.unavailable[key] = true
		slog.Warn(fmt.Sprintf("Fallback skip: %s/%s is not locally usable (%s); suppressing for this session", provider, model, reason))
		return true, nil
	}
	rt := s.Route()
	current := NewBackendIdentity(rt.Provider, rt.Model, rt.BaseURL)
	if s.r.ShouldSkipCandidate(ctx, NewBackendIdentity(provider, model, fb.BaseURL), current, ScopeModel) {
		slog.Warn(fmt.Sprintf("Fallback skip: chain entry %s/%s resolves to the same backend as the current one (%s)", provider, model, firstOf(current.BaseURL, current.Provider)))
		return true, nil
	}
	return false, nil
}

type Binding struct {
	FromModel     string
	FromProvider  string
	Model         string
	Provider      string
	ContextLength int
	ContextKnown  bool
}

type FallbackResult struct {
	Binding
	Reason   Class
	Notice   string
	Cooldown time.Duration
}

func (s *Session) Fallback(ctx context.Context, reason Class, resetAt time.Time) (*FallbackResult, bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if s.switchDeferredByReset(ctx, reason, resetAt) {
		return nil, false, nil
	}
	cooldown, armed := s.armRateLimitCooldown(reason, resetAt)
	for {
		if s.rec.index >= len(s.rec.chain) {
			if len(s.rec.chain) > 0 && !rateLimitFailover(reason) {
				if until := time.Now().Add(fallbackExhaustedCooldown); until.After(s.rec.limitedUntil) {
					s.rec.limitedUntil = until
				}
			}
			return nil, false, nil
		}
		fb := s.rec.chain[s.rec.index]
		s.rec.index++
		key := fallbackEntryKey(fb)
		provider, model := strings.ToLower(strings.TrimSpace(fb.Provider)), strings.TrimSpace(fb.Model)
		skip, err := s.skipFallbackCandidate(ctx, fb, key, provider, model)
		if err != nil {
			return nil, false, err
		}
		if skip {
			continue
		}
		b, bound, err := s.bindEntry(ctx, fb, provider, model)
		if err != nil {
			if provider == portal.Provider {
				s.rec.unavailable[key] = true
			}
			slog.Error(fmt.Sprintf("Failed to activate fallback %s: %s", model, err))
			continue
		}
		if !bound {
			slog.Warn(fmt.Sprintf("Fallback to %s failed: provider not configured", provider))
			s.rec.unavailable[key] = true
			continue
		}
		notice := fmt.Sprintf("⚠️ Model fallback: %s via %s unavailable (%s); using %s via %s.",
			b.FromModel, b.FromProvider, FallbackReasonText(reason), b.Model, provider)
		if armed {
			remaining := max(0, int(math.Ceil(time.Until(s.rec.limitedUntil).Seconds())))
			notice += fmt.Sprintf(" Primary retry eligible in ~%d s; recovery is not guaranteed.", remaining)
		}
		s.rec.providerActive = true
		s.rec.fbModel, s.rec.fbProvider, s.rec.hasFBRoute = b.Model, provider, true
		s.logFallbackActivated(reason, b.FromModel, b.FromProvider, b.Model, provider)
		res := &FallbackResult{Binding: b, Reason: reason, Notice: notice}
		if armed {
			res.Cooldown = time.Duration(cooldown) * time.Second
		}
		return res, true, nil
	}
}

func (s *Session) logFallbackActivated(reason Class, oldModel, oldProvider, model, provider string) {
	if reason != ClassBilling {
		slog.Info(fmt.Sprintf("Fallback activated: %s → %s (%s)", oldModel, model, provider))
		return
	}
	profile := home.DefaultProfile
	if name, ok := home.ProfileName(s.r.c.o.Home); ok && name != "" {
		profile = name
	}
	remedy := "icebath model"
	if profile != home.DefaultProfile {
		remedy = "icebath -p " + profile + " model"
	}
	slog.Warn(fmt.Sprintf("Profile %s: %s via %s refused for billing/credits — using fallback %s via %s. Top up credits, or run `%s` to pick a model this account can use.",
		profile, oldModel, oldProvider, model, provider, remedy))
}

func (s *Session) BindEntry(ctx context.Context, entry config.FallbackEntry) (*Binding, bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	b, bound, err := s.bindEntry(ctx, entry, strings.ToLower(strings.TrimSpace(entry.Provider)), strings.TrimSpace(entry.Model))
	if err != nil || !bound {
		return nil, bound, err
	}
	return &b, true, nil
}

func (s *Session) bindEntry(ctx context.Context, entry config.FallbackEntry, provider, model string) (Binding, bool, error) {
	x, err := s.r.begin(ctx)
	if err != nil {
		return Binding{}, false, err
	}
	baseHint := strings.TrimSpace(entry.BaseURL)
	keyHint, err := s.r.FallbackEntryAPIKey(ctx, entry)
	if err != nil {
		return Binding{}, false, err
	}
	explicit, mode, err := s.fallbackModeHint(x, entry, provider, baseHint)
	if err != nil {
		return Binding{}, false, err
	}
	if baseHint != "" && providers.HostMatches(baseHint, "ollama.com") && keyHint == "" {
		if keyHint, err = s.r.c.o.Secrets.Get(ctx, "OLLAMA_API_KEY", ""); err != nil {
			return Binding{}, false, err
		}
	}
	t := Target{Provider: provider, Model: model, BaseURL: baseHint}
	if keyHint != "" {
		t.APIKey = auth.NewSecret(keyHint)
	}
	resolved, err := s.r.Resolve(ctx, t)
	if err != nil {
		var disabled *ProviderDisabledError
		var unavailable *LocalRuntimeUnavailableError
		if IsAuthError(err) || errors.As(err, &disabled) || errors.As(err, &unavailable) {
			slog.Debug("fallback provider has no usable client", "provider", provider, "error", err)
			return Binding{}, false, nil
		}
		return Binding{}, false, err
	}
	rt := cloneRoute(resolved)
	base := rt.BaseURL
	if provider == "moa" {
		base, mode = "moa://local", wire.ModeChat
	} else {
		model = s.r.c.o.Catalog.NormalizeModelForProvider(ctx, model, provider)
		switch {
		case s.r.c.o.Providers.IsActualRoute(ctx, provider, base):
			mode = wire.ModeChat
		case !explicit && mode == wire.ModeChat:
			if mode, err = s.fallbackModeResolved(x, provider, model, base); err != nil {
				return Binding{}, false, err
			}
		}
	}
	if provider == "bedrock" && (rt.Mode == wire.ModeAnthropic || rt.Mode == wire.ModeBedrock) && rt.Credential.Kind == auth.CredSigned {
		mode = rt.Mode
	}
	if mode == wire.ModeAnthropic && !keyPresent(rt.Credential) && provider == "anthropic" {
		if tok, ok := s.r.c.o.Auth.ResolveAnthropicToken(ctx, model); ok {
			rt.Credential = staticKey(tok.Reveal(), rt.Credential.Source)
		}
	}
	old := s.Route()
	rt.Model, rt.Provider, rt.Requested, rt.BaseURL, rt.Mode = model, provider, provider, base, mode
	pool := old.Pool
	if pool != nil {
		if pp := strings.ToLower(strings.TrimSpace(pool.Provider())); pp != "" && pp != provider {
			slog.Info(fmt.Sprintf("Fallback to %s/%s: clearing primary credential pool (pool_provider=%s) to prevent cross-provider contamination", provider, model, pp))
			pool = nil
		}
	}
	if pool == nil {
		loaded, lerr := s.r.c.o.Auth.Pool(ctx, provider)
		switch {
		case lerr != nil:
			slog.Debug(fmt.Sprintf("Fallback to %s/%s: could not attach credential pool: %s", provider, model, lerr))
		case loaded != nil && loaded.HasCredentials():
			pool = loaded
			slog.Info(fmt.Sprintf("Fallback to %s/%s: attached fallback credential pool", provider, model))
		}
	}
	rt.Pool = pool
	echo := entry.ReasoningEcho != nil && *entry.ReasoningEcho
	s.mu.Lock()
	s.route = rt
	s.echoOptIn = echo
	s.o.NoEncryptedReplay = false
	s.mu.Unlock()
	s.exec.resetStreak()
	s.rec.activated = true
	s.syncEntryID()
	if _, _, lerr := s.ensureLMStudioLoaded(ctx, rt, nil); lerr != nil {
		return Binding{}, false, lerr
	}
	b := Binding{FromModel: old.Model, FromProvider: old.Provider, Model: model, Provider: provider}
	if b.ContextLength, b.ContextKnown, err = s.routeContextLength(ctx, rt, 0); err != nil {
		return Binding{}, false, err
	}
	s.reresolveReasoning(ctx, model)
	s.rescopeExtraBody(ctx, old, rt)
	return b, true, nil
}

func (s *Session) reresolveReasoning(ctx context.Context, model string) {
	cfg, err := s.r.c.o.Config.LoadReadonly(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to resolve reasoning_config for fallback %s; keeping current: %s", model, err))
		return
	}
	r := ResolveReasoningConfig(cfg, model)
	s.mu.Lock()
	s.o.Reasoning = r
	s.mu.Unlock()
	slog.Info(fmt.Sprintf("Fallback %s: reasoning_config resolved: %s", model, reasoningText(r)))
}

func reasoningText(r *wire.Reasoning) string {
	switch {
	case r == nil:
		return "None"
	case !r.Enabled:
		return "{'enabled': False}"
	}
	return "{'enabled': True, 'effort': '" + r.Effort + "'}"
}

func (s *Session) rescopeExtraBody(ctx context.Context, old, rt *Route) {
	custom, err := s.customEndpoints(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to resolve extra_body for fallback %s; keeping current: %s", rt.Model, err))
		return
	}
	oldEB := customProviderExtraBody(old.Provider, old.Model, old.BaseURL, custom)
	s.mu.Lock()
	defer s.mu.Unlock()
	overrides := cloneOverrides(s.o.Overrides)
	if existing, ok := objectMembers(overrides["extra_body"]); ok && len(oldEB) > 0 {
		scrubbed := map[string]json.RawMessage{}
		for k, v := range existing {
			if ov, had := oldEB[k]; had && jsonEqual(v, ov) {
				continue
			}
			scrubbed[k] = v
		}
		if len(scrubbed) > 0 {
			overrides["extra_body"] = objectJSON(scrubbed)
		} else {
			delete(overrides, "extra_body")
		}
	}
	if eb := customProviderExtraBody(rt.Provider, rt.Model, rt.BaseURL, custom); len(eb) > 0 {
		merged := cloneOverrides(eb)
		if existing, ok := objectMembers(overrides["extra_body"]); ok {
			for k, v := range existing {
				merged[k] = v
			}
		}
		overrides["extra_body"] = objectJSON(merged)
	}
	s.o.Overrides = overrides
	ebText := "None"
	if raw, ok := overrides["extra_body"]; ok {
		ebText = string(raw)
	}
	slog.Info(fmt.Sprintf("Fallback %s: extra_body resolved: %s", rt.Model, ebText))
}

func jsonEqual(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

func objectJSON(m map[string]json.RawMessage) json.RawMessage {
	d := &wire.Dict{}
	for _, k := range sortedKeys(m) {
		d.Set(k, m[k])
	}
	return d.JSON()
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func customProviderExtraBody(provider, model, baseURL string, entries []config.CustomEndpoint) map[string]json.RawMessage {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p != "custom" && !strings.HasPrefix(p, "custom:") {
		return nil
	}
	_, filter, _ := strings.Cut(p, ":")
	filter = strings.TrimSpace(filter)
	target := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if target == "" {
		return nil
	}
	var fallback map[string]json.RawMessage
	for _, e := range entries {
		keys := map[string]bool{strings.ToLower(strings.TrimSpace(e.Text("provider_key"))): true, strings.ToLower(strings.TrimSpace(e.Text("name"))): true}
		if filter != "" && !keys[filter] {
			continue
		}
		if strings.TrimRight(strings.TrimSpace(e.Text("base_url")), "/") != target {
			continue
		}
		eb := objectMapOfNode(yamlChild(e.Node(), "extra_body"))
		if len(eb) == 0 {
			continue
		}
		if strings.TrimSpace(e.Text("model")) != "" {
			if customModelMatches(model, e) {
				return eb
			}
		} else if fallback == nil {
			fallback = eb
		}
	}
	return fallback
}

func customModelMatches(model string, e config.CustomEndpoint) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	var listed []string
	if n := yamlChild(e.Node(), "models"); n != nil {
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				listed = append(listed, strings.ToLower(strings.TrimSpace(n.Content[i].Value)))
			}
		case yaml.SequenceNode:
			for _, item := range n.Content {
				listed = append(listed, strings.ToLower(strings.TrimSpace(item.Value)))
			}
		}
	}
	for _, m := range listed {
		if m == want {
			return true
		}
	}
	declared := strings.ToLower(strings.TrimSpace(e.Text("model")))
	return (declared == "" && len(listed) == 0) || declared == want
}

func (s *Session) fallbackModeHint(x *resolution, entry config.FallbackEntry, provider, baseHint string) (bool, wire.Mode, error) {
	raw := entry.APIMode
	if raw == "" {
		raw = entry.Transport
	}
	if m := x.parseMode(raw); m != "" {
		return true, m, nil
	}
	if provider != "" && provider != "custom" && provider != "moa" {
		nc, err := x.namedCustomProvider(provider, nil, false)
		if err != nil {
			return false, "", err
		}
		if nc != nil && nc.APIMode != "" {
			return true, nc.APIMode, nil
		}
	}
	if provider == "anthropic" || (baseHint != "" && anthropicWireURL(baseHint)) {
		return false, wire.ModeAnthropic, nil
	}
	return false, wire.ModeChat, nil
}

func anthropicWireURL(url string) bool {
	m, ok := providers.HostMandatedMode(url)
	return ok && m == wire.ModeAnthropic
}

func (s *Session) fallbackModeResolved(x *resolution, provider, model, base string) (wire.Mode, error) {
	if provider == "openai-codex" {
		return wire.ModeResponses, nil
	}
	reg := s.r.c.o.Providers
	if family, ok := x.openCodeFamilyForCustom(provider, base); ok {
		return reg.OpenCodeModelMode(x.ctx, family, model), nil
	}
	if provider == portal.Provider || provider == "icebath-portal" {
		return x.portalMode(model)
	}
	if anthropicWireURL(base) {
		return wire.ModeAnthropic, nil
	}
	if providers.HostMatches(strings.ToLower(base), "openai.azure.com") {
		return wire.ModeChat, nil
	}
	if providers.URLHostname(base) == "api.openai.com" || s.providerModelRequiresResponses(x, model, provider) {
		return wire.ModeResponses, nil
	}
	host := providers.URLHostname(base)
	if provider == "bedrock" || (strings.HasPrefix(host, "bedrock-runtime.") && providers.HostMatches(base, "amazonaws.com")) {
		return wire.ModeBedrock, nil
	}
	return wire.ModeChat, nil
}

func (s *Session) providerModelRequiresResponses(x *resolution, model, provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	reg := s.r.c.o.Providers
	if p == portal.Provider || p == "custom" || reg.IsActualRoute(x.ctx, provider, "") {
		return false
	}
	if x.isExternalProcess(p) {
		return false
	}
	if p != "" {
		if prof, ok := reg.Get(x.ctx, p); ok {
			if m, ok := reg.ModeForModel(x.ctx, prof, model, nil); ok {
				return m == wire.ModeResponses
			}
		}
	}
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower[strings.LastIndex(lower, "/")+1:], "gpt-5")
}
