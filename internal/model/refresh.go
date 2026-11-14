package model

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/model/wire/anthropic"
)

const portalKeyAdoptSkew = 180 * time.Second

func validCredentialPair(key auth.Secret, baseURL string) bool {
	return strings.TrimSpace(key.Reveal()) != "" && strings.TrimSpace(baseURL) != ""
}

func (s *Session) adoptCredentials(key auth.Secret, baseURL string, expires time.Time) {
	rt := cloneRoute(s.Route())
	rt.Credential = staticKey(strings.TrimSpace(key.Reveal()), rt.Credential.Source)
	rt.Credential.ExpiresAt = expires
	rt.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	s.setRoute(rt)
}

func (s *Session) RefreshCodex(ctx context.Context, force bool) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	rt := s.Route()
	if rt.Mode != wire.ModeResponses || (rt.Provider != auth.ProviderCodex && rt.Provider != "xai-oauth") {
		return false, nil
	}
	store := s.r.c.o.Auth
	resolve := store.ResolveCodex
	if rt.Provider == "xai-oauth" {
		resolve = store.ResolveXAI
	}
	singleton, err := resolve(ctx, auth.ResolveOptions{NoRefresh: true})
	if err != nil {
		slog.Debug(fmt.Sprintf("%s singleton read failed: %s", rt.Provider, err))
		return false, nil
	}
	singletonKey, old := strings.TrimSpace(singleton.APIKey.Reveal()), strings.TrimSpace(keyText(rt.Credential))
	if singletonKey != "" && old != "" && singletonKey != old {
		slog.Debug(fmt.Sprintf("%s singleton tokens differ from the active api_key; skipping singleton force-refresh to avoid silent account swap. Reactive credential rotation should go through the pool.", rt.Provider))
		return false, nil
	}
	creds, err := resolve(ctx, auth.ResolveOptions{ForceRefresh: force})
	if err != nil {
		slog.Debug(fmt.Sprintf("%s credential refresh failed: %s", rt.Provider, err))
		return false, nil
	}
	if !validCredentialPair(creds.APIKey, creds.BaseURL) {
		return false, nil
	}
	if old != "" && strings.TrimSpace(creds.APIKey.Reveal()) == old {
		slog.Debug(fmt.Sprintf("%s credential refresh returned the same token; refresh likely failed silently", rt.Provider))
		return false, nil
	}
	s.adoptCredentials(creds.APIKey, creds.BaseURL, msTime(creds.ExpiresAtMS))
	return true, nil
}

func (s *Session) RefreshPortal(ctx context.Context, force bool) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.refreshPortal(ctx, force, "")
}

func (s *Session) portalTimeout(ctx context.Context) time.Duration {
	raw, err := s.r.c.o.Secrets.Get(ctx, "ICEBATH_PORTAL_TIMEOUT_SECONDS", "")
	if err == nil {
		if secs, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64); perr == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 15 * time.Second
}

func (s *Session) refreshPortal(ctx context.Context, force bool, requireAccount string) (bool, error) {
	rt := s.Route()
	if rt.Provider != portal.Provider || (rt.Mode != wire.ModeChat && rt.Mode != wire.ModeAnthropic) {
		return false, nil
	}
	current := keyText(rt.Credential)
	creds, err := s.r.c.o.Portal.RuntimeCredentials(ctx, portal.ResolveOptions{Timeout: s.portalTimeout(ctx), ForceRefresh: force,
		StaleAccessToken: auth.NewSecret(current)})
	if err != nil {
		slog.Debug(fmt.Sprintf("Icebath Portal credential refresh failed: %s", err))
		return false, nil
	}
	if !validCredentialPair(creds.APIKey, creds.BaseURL) {
		return false, nil
	}
	if strings.TrimSpace(creds.APIKey.Reveal()) == strings.TrimSpace(current) &&
		strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/") == strings.TrimRight(strings.TrimSpace(rt.BaseURL), "/") {
		return false, nil
	}
	if requireAccount != "" {
		if sub, _ := jwtSubject(creds.APIKey.Reveal()); sub != requireAccount {
			slog.Info("Icebath Portal pre-expiry adoption skipped: the store's key belongs to a different account than the one in hand; keeping the current credential.")
			return false, nil
		}
	}
	s.adoptCredentials(creds.APIKey, creds.BaseURL, isoTime(creds.ExpiresAt))
	return true, nil
}

func jwtSubject(token string) (string, *float64) {
	raw, ok := auth.JWTClaims(token)
	if !ok {
		return "", nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return "", nil
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		if n, isNum := claims["sub"].(float64); isNum {
			sub = strconv.FormatFloat(n, 'f', -1, 64)
		}
	}
	if exp, isNum := claims["exp"].(float64); isNum {
		return sub, &exp
	}
	return sub, nil
}

func (s *Session) AdoptPortalKeyBeforeExpiry(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	rt := s.Route()
	key := keyText(rt.Credential)
	if rt.Provider != portal.Provider || key == "" {
		return false, nil
	}
	account, exp := jwtSubject(key)
	if account == "" || exp == nil || *exp-float64(time.Now().UnixNano())/1e9 > portalKeyAdoptSkew.Seconds() {
		return false, nil
	}
	return s.refreshPortal(ctx, false, account)
}

func (s *Session) envPreferDotenv(ctx context.Context, name string) (string, error) {
	return s.r.envPreferDotenv(ctx, name)
}

func (s *Session) resolveEnvCredentials(ctx context.Context) (key, baseURL, defaultBase string, ok bool, err error) {
	rt := s.Route()
	reg := s.r.c.o.Providers
	row, known := reg.RegistryRow(ctx, rt.Provider)
	switch {
	case known && row.AuthType == "api_key" && len(row.APIKeyEnvVars) > 0:
		for _, name := range row.APIKeyEnvVars {
			if key, err = s.envPreferDotenv(ctx, name); err != nil {
				return "", "", "", false, err
			}
			if key != "" {
				break
			}
		}
		if key == "" {
			return "", "", "", false, nil
		}
		envURL := ""
		if row.BaseURLEnvVar != "" {
			v, err := s.envPreferDotenv(ctx, row.BaseURLEnvVar)
			if err != nil {
				return "", "", "", false, err
			}
			envURL = strings.TrimRight(v, "/")
		}
		defaultBase = strings.TrimRight(strings.TrimSpace(row.InferenceBaseURL), "/")
		baseURL = firstOf(envURL, defaultBase)
		switch rt.Provider {
		case "actual":
			x, err := s.r.begin(ctx)
			if err != nil {
				return "", "", "", false, err
			}
			baseURL = auth.NormalizeActualBaseURL(firstOf(x.configBaseURLFor(x.model(), "actual"), baseURL))
		case "kimi-coding", "zai":
			if p, ok := reg.Get(ctx, rt.Provider); ok {
				if d, ok := p.(providers.BaseURLDeriver); ok {
					if derived, ok := d.RuntimeBaseURL(ctx, auth.NewSecret(key), row.InferenceBaseURL, envURL); ok {
						baseURL = strings.TrimRight(derived, "/")
					}
				}
			}
		}
	case rt.Provider == "custom":
		x, err := s.r.begin(ctx)
		if err != nil {
			return "", "", "", false, err
		}
		nc, err := x.namedCustomProvider(rt.Requested, nil, false)
		if err != nil {
			return "", "", "", false, err
		}
		if nc == nil {
			return "", "", "", false, nil
		}
		if keyEnv := strings.TrimSpace(nc.KeyEnv); keyEnv != "" {
			if key, err = s.envPreferDotenv(ctx, keyEnv); err != nil {
				return "", "", "", false, err
			}
		}
		if key == "" {
			return "", "", "", false, nil
		}
		baseURL = strings.TrimRight(strings.TrimSpace(nc.BaseURL), "/")
		defaultBase = baseURL
	default:
		return "", "", "", false, nil
	}
	if baseURL == "" {
		return "", "", "", false, nil
	}
	return key, baseURL, defaultBase, true, nil
}

func (s *Session) shouldAdoptEnv(rt *Route, key, baseURL, defaultBase string) bool {
	currentBase := strings.TrimRight(strings.TrimSpace(rt.BaseURL), "/")
	currentKey := keyText(rt.Credential)
	unchanged := baseURL == currentBase && key == currentKey
	prev := s.rec.envSeen
	if prev == nil {
		poolRotated := key != currentKey && rt.Pool != nil && s.rec.entryID != ""
		return currentBase == defaultBase && !unchanged && !poolRotated
	}
	seen := baseURL == prev.baseURL && key == prev.key.Reveal()
	return !seen && (currentBase == defaultBase || currentBase == prev.baseURL) && !unchanged
}

func (s *Session) RefreshEnvCredentials(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	rt := s.Route()
	if rt.Mode != wire.ModeChat || s.rec.activated {
		return false, nil
	}
	key, baseURL, defaultBase, ok, err := s.resolveEnvCredentials(ctx)
	if err != nil || !ok {
		return false, err
	}
	if !s.shouldAdoptEnv(rt, key, baseURL, defaultBase) {
		s.rec.envSeen = &envSeen{baseURL: baseURL, key: auth.NewSecret(key)}
		return false, nil
	}
	n := cloneRoute(rt)
	n.Credential = staticKey(key, rt.Credential.Source)
	n.BaseURL = baseURL
	s.setRoute(n)
	s.syncEntryID()
	s.rec.envSeen = &envSeen{baseURL: baseURL, key: auth.NewSecret(key)}
	slog.Info(fmt.Sprintf("Applied updated .env credentials for %s: endpoint %s", rt.Provider, baseURL))
	return true, nil
}

func (s *Session) RefreshVertex(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	rt := s.Route()
	if rt.Mode != wire.ModeChat || rt.Provider != "vertex" {
		return false, nil
	}
	owner, ok := s.r.c.o.Providers.VertexOwner()
	if !ok {
		return false, nil
	}
	v, err := owner.Vertex(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Vertex credential refresh failed: %s", err))
		return false, nil
	}
	tok, base, err := v.Config(ctx, "", "")
	if err != nil {
		slog.Debug(fmt.Sprintf("Vertex credential refresh failed: %s", err))
		return false, nil
	}
	if !validCredentialPair(tok.Value, base) {
		return false, nil
	}
	s.adoptCredentials(tok.Value, base, tok.Expiry)
	slog.Info("Vertex AI OAuth token refreshed")
	return true, nil
}

func (s *Session) refreshProfile(ctx context.Context) (providers.Profile, bool) {
	rt := s.Route()
	profiles := s.r.c.o.Providers.RouteProfiles(ctx, providers.HookRefreshRuntime, strings.ToLower(strings.TrimSpace(rt.Provider)), rt.BaseURL)
	if len(profiles) == 0 {
		return nil, false
	}
	return profiles[0], true
}

func (s *Session) HasCredentialRefreshProfile(ctx context.Context) bool {
	_, ok := s.refreshProfile(s.r.bind(ctx))
	return ok
}

func (s *Session) StaleCredential(ctx context.Context, status int, message string) bool {
	ctx = s.r.bind(ctx)
	p, ok := s.refreshProfile(ctx)
	return ok && s.r.c.o.Providers.IsStale(ctx, p, status, message)
}

func (s *Session) RefreshProfileCredential(ctx context.Context, reason string) (bool, string, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	p, ok := s.refreshProfile(ctx)
	if !ok {
		return false, "", nil
	}
	reg := s.r.c.o.Providers
	key, base, ok := reg.RefreshRuntime(ctx, p, reason)
	if !ok || key.Empty() {
		return false, "", nil
	}
	n := cloneRoute(s.Route())
	n.Credential = staticKey(key.Reveal(), n.Credential.Source)
	if base != "" {
		n.BaseURL = strings.TrimRight(base, "/")
	}
	s.setRoute(n)
	notice := reg.RefreshNotice(ctx, p, reason)
	if notice == "" {
		spec := p.Spec()
		name := firstOf(spec.DisplayName, spec.Name)
		if reason == "stale" {
			notice = name + " credential renewed after a stale-credential error. Retrying request..."
		} else {
			notice = name + " credentials refreshed after 401. Retrying request..."
		}
	}
	return true, "🔐 " + notice, nil
}

func (s *Session) RefreshAnthropic(ctx context.Context) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.refreshAnthropic(ctx), nil
}

func (s *Session) refreshAnthropic(ctx context.Context) bool {
	rt := s.Route()
	base := anthropic.NormalizeBaseURL(rt.BaseURL)
	if rt.Mode != wire.ModeAnthropic || rt.Provider != "anthropic" || providers.HostMatches(base, "azure.com") {
		return false
	}
	official := base == "" || providers.HostMatches(base, "anthropic.com") || providers.HostMatches(base, "claude.com")
	current := keyText(rt.Credential)
	if !official && !(strings.HasPrefix(current, "sk-ant-") || auth.AnthropicRouteIsOAuth(ctx, base, rt.Credential, rt.Provider)) {
		return false
	}
	tok, ok := s.r.c.o.Auth.ResolveAnthropicToken(ctx, rt.Model)
	fresh := strings.TrimSpace(tok.Reveal())
	if !ok || fresh == "" || fresh == current {
		return false
	}
	n := cloneRoute(rt)
	n.Credential = staticKey(fresh, rt.Credential.Source)
	s.setRoute(n)
	if p := s.rec.primary.Route(); p != nil && keyText(p.Credential) == current {
		p.Credential = staticKey(fresh, p.Credential.Source)
	}
	return true
}

func (s *Session) refreshAnthropicBeforeCall(ctx context.Context) {
	if s.Route().Mode != wire.ModeAnthropic {
		return
	}
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	s.refreshAnthropic(ctx)
}
