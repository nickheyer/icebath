package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const codexQuotaProbeInterval = 300 * time.Second

type Restored struct {
	Model            string
	Provider         string
	PreviousModel    string
	PreviousProvider string
	Notice           string
}

type primaryScope struct {
	s        *Session
	ctx      context.Context
	state    *RouteState
	provider string
	model    string
	baseURL  string
}

func (p *primaryScope) matches(provider string) bool {
	return p.s.r.c.o.Auth.PoolMatchesProvider(p.ctx, provider, p.provider, p.baseURL)
}

func (p *primaryScope) matchesPool(pool *auth.Pool) bool {
	return pool == nil || p.matches(pool.Provider())
}

func (p *primaryScope) load() (*auth.Pool, error) {
	pool, err := p.s.r.c.o.Auth.LoadRuntimePool(p.ctx, p.provider, p.baseURL, p.state.route.Requested, true, p.state.owner)
	if err != nil {
		return nil, err
	}
	if pool != nil && p.matches(pool.Provider()) {
		return pool, nil
	}
	return nil, nil
}

func (s *Session) RestorePrimary(ctx context.Context) (*Restored, bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if !s.rec.activated {
		s.rec.index = 0
		s.revertCredentialRotation(ctx)
		return nil, false, nil
	}
	st := s.rec.primary
	if st == nil || st.route == nil {
		return nil, false, errors.New("model: the session has no primary route to restore")
	}
	p := &primaryScope{s: s, ctx: ctx, state: st, provider: strings.ToLower(strings.TrimSpace(st.route.Provider)),
		model: strings.TrimSpace(st.route.Model), baseURL: st.route.BaseURL}
	if p.model != "" && (s.entitlementRejected(ctx, p.provider, p.model) || s.r.c.o.Catalog.IsKnownNonChatModel(p.model)) {
		return nil, false, nil
	}
	if s.primaryQuotaReopenedEarly(p) {
		s.rec.limitedUntil = time.Time{}
	}
	if time.Now().Before(s.rec.limitedUntil) {
		return nil, false, nil
	}
	blocked, prefetched, didPrefetch := s.primaryResetGateBlocks(p)
	if blocked {
		return nil, false, nil
	}
	s.rec.waitLogged = false
	cur := s.Route()
	prevModel, prevProvider := cur.Model, cur.Provider
	if s.rec.hasFBRoute {
		prevModel, prevProvider = s.rec.fbModel, s.rec.fbProvider
	}
	prevModel, prevProvider = firstOf(prevModel, "unknown"), firstOf(prevProvider, "unknown")
	providerActive := s.rec.providerActive
	if err := s.reinstallPrimary(p, prefetched, didPrefetch); err != nil {
		slog.Warn(fmt.Sprintf("Failed to restore primary runtime: %s", err))
		return nil, false, nil
	}
	rt := s.Route()
	slog.Info(fmt.Sprintf("Primary runtime restored for new turn: %s (%s)", rt.Model, rt.Provider))
	s.rec.providerActive, s.rec.hasFBRoute, s.rec.fbModel, s.rec.fbProvider = false, false, "", ""
	res := &Restored{Model: rt.Model, Provider: rt.Provider, PreviousModel: prevModel, PreviousProvider: prevProvider}
	if providerActive {
		res.Notice = fmt.Sprintf("✅ Primary model restored: %s via %s; fallback %s via %s is no longer active.", rt.Model, rt.Provider, prevModel, prevProvider)
	}
	return res, true, nil
}

func (s *Session) ArmPrimaryRestore(st *RouteState) {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	if st != nil && st.route != nil {
		s.rec.primary = st
	}
	s.rec.activated = true
	s.rec.limitedUntil = time.Time{}
}

func (s *Session) PrimaryState() *RouteState {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.rec.primary
}

func (s *Session) reinstallPrimary(p *primaryScope, prefetched *auth.Pool, didPrefetch bool) error {
	s.reinstallRuntime(p.ctx, p.state)
	if err := s.rebindPrimaryPool(p, prefetched, didPrefetch); err != nil {
		return err
	}
	s.rec.activated = false
	s.rec.index = 0
	s.rec.backoffs = 0
	return nil
}

func (s *Session) rebindPrimaryPool(p *primaryScope, prefetched *auth.Pool, didPrefetch bool) error {
	rt := cloneRoute(s.Route())
	pool := rt.Pool
	if pool != nil && strings.TrimSpace(pool.Provider()) != "" && !p.matches(pool.Provider()) {
		pool = nil
		s.rec.entryID = ""
		if didPrefetch {
			pool = prefetched
		} else {
			loaded, err := p.load()
			if err != nil {
				slog.Warn(fmt.Sprintf("Restore could not reload primary credential pool for %s: %s", p.provider, err))
			}
			pool = loaded
		}
		rt.Pool = pool
		s.setRoute(rt)
	}
	s.rec.entryID = ""
	if pool == nil {
		return nil
	}
	avail, err := pool.HasAvailable(p.ctx, p.model)
	if err != nil || !avail {
		return err
	}
	entry, ok, err := pool.Select(p.ctx, p.model)
	if err != nil || !ok {
		return err
	}
	if entry.RuntimeAPIKey().Empty() && entry.AccessToken.Empty() {
		return nil
	}
	label := firstOf(entry.Label, "?")
	if p.matches(entry.Provider) {
		s.swapCredential(p.ctx, entry)
		slog.Info(fmt.Sprintf("Restore re-selected pool entry %s (%s)", firstOf(entry.ID, "?"), label))
		return nil
	}
	slog.Info(fmt.Sprintf("Restore skipped pool entry %s (%s): provider %s does not match primary provider %s", firstOf(entry.ID, "?"), label,
		firstOf(strings.ToLower(strings.TrimSpace(entry.Provider)), "?"), firstOf(p.provider, "?")))
	return nil
}

func (s *Session) primaryQuotaReopenedEarly(p *primaryScope) bool {
	if p.provider != auth.ProviderCodex {
		return false
	}
	now := time.Now()
	if !s.rec.reopenCheckedAt.IsZero() && now.Sub(s.rec.reopenCheckedAt) < codexQuotaProbeInterval {
		return false
	}
	s.rec.reopenCheckedAt = now
	pool := s.Route().Pool
	if pool == nil || !p.matches(pool.Provider()) {
		loaded, err := p.load()
		if err != nil {
			slog.Debug("Early quota-reopen check failed; keeping the cooldown", "error", err)
			return false
		}
		pool = loaded
	}
	if pool == nil {
		return false
	}
	until, ok, err := pool.NextAvailableAt(p.ctx, p.model)
	if err != nil {
		slog.Debug("Early quota-reopen check failed; keeping the cooldown", "error", err)
		return false
	}
	if !ok || !until.After(now) {
		return false
	}
	lifted, err := pool.LiftReopenedCooldowns(p.ctx, p.model)
	if err != nil {
		slog.Debug("Early quota-reopen check failed; keeping the cooldown", "error", err)
		return false
	}
	return lifted
}

func (s *Session) primaryResetGateBlocks(p *primaryScope) (bool, *auth.Pool, bool) {
	var prefetched *auth.Pool
	did := false
	pool := s.Route().Pool
	if !p.matchesPool(pool) {
		loaded, err := p.load()
		if err != nil {
			slog.Debug("Reset-aware restore gate failed; falling back to per-turn retry", "error", err)
			return false, nil, false
		}
		prefetched, pool, did = loaded, loaded, true
	}
	if pool == nil {
		return false, prefetched, did
	}
	next, ok, err := pool.NextAvailableAt(p.ctx, p.model)
	if err != nil {
		slog.Debug("Reset-aware restore gate failed; falling back to per-turn retry", "error", err)
		return false, prefetched, did
	}
	if ok && next.After(time.Now()) {
		if !s.rec.waitLogged {
			s.rec.waitLogged = true
			cur := s.Route()
			slog.Info(fmt.Sprintf("Primary %s rate-limited until %s; staying on fallback %s/%s until the reset elapses",
				firstOf(p.provider, "?"), next.Local().Format("2006-01-02T15:04:05"), cur.Provider, cur.Model))
		}
		return true, prefetched, did
	}
	return false, prefetched, did
}

func (s *Session) revertCredentialRotation(ctx context.Context) {
	revert := s.rec.revertID
	if revert == "" {
		return
	}
	rt := s.Route()
	if rt.Pool == nil || s.rec.entryID == revert {
		s.rec.revertID = ""
		return
	}
	entry, ok, err := rt.Pool.Reclaim(ctx, revert, rt.Model)
	if err != nil {
		slog.Warn(fmt.Sprintf("Credential revert check failed: %s", err))
		return
	}
	if !ok {
		return
	}
	if s.swapCredential(ctx, entry) {
		slog.Info(fmt.Sprintf("Credential %s (%s) available again — reverted pool rotation", firstOf(entry.ID, "?"), firstOf(entry.Label, "?")))
	}
	s.rec.revertID = ""
}

func transientTransport(err error) (string, bool) {
	var ae *wire.APIError
	if err == nil || errors.As(err, &ae) {
		return "", false
	}
	var he *httpclient.Error
	if errors.As(err, &he) {
		switch {
		case errors.Is(he.Err, httpclient.ErrTimeout) && he.Phase == httpclient.PhaseConnect:
			return "connect timeout", true
		case errors.Is(he.Err, httpclient.ErrTimeout):
			return "read timeout", true
		case connectionDropped(err):
			return "connection drop", true
		case he.Phase == httpclient.PhaseConnect || he.Phase == httpclient.PhaseTLS:
			return "connect error", true
		}
		return "read error", true
	}
	if errors.Is(err, httpclient.ErrTimeout) {
		return "request timeout", true
	}
	if connectionDropped(err) {
		return "connection drop", true
	}
	var ne net.Error
	var op *net.OpError
	var errno syscall.Errno
	var sys *os.SyscallError
	if errors.As(err, &op) || errors.As(err, &ne) || errors.As(err, &errno) || errors.As(err, &sys) || certVerifyFailure(err) || tlsFailure(err) {
		return "connection error", true
	}
	return "", false
}

func (s *Session) RecoverTransport(ctx context.Context, err error, retryCount int, notify func(text string)) (bool, error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	label, transient := transientTransport(err)
	cur := s.Route()
	if s.rec.activated || !transient || providers.HostMatches(strings.ToLower(cur.BaseURL), "openrouter.ai") {
		return false, nil
	}
	provider := strings.ToLower(strings.TrimSpace(cur.Provider))
	if (provider == portal.Provider || provider == "icebath-portal") && cur.Mode != wire.ModeAnthropic {
		return false, nil
	}
	st := s.rec.primary
	if st == nil || st.route == nil {
		slog.Warn("Primary transport recovery failed: the session has no primary route")
		return false, nil
	}
	if client, cerr := s.r.endpointClient(ctx, cur.BaseURL); cerr == nil {
		client.CloseIdleConnections()
	}
	s.reinstallFields(ctx, st)
	client, cerr := s.r.endpointClient(ctx, st.route.BaseURL)
	if cerr != nil {
		slog.Warn(fmt.Sprintf("Primary transport recovery failed: %s", cerr))
		return false, nil
	}
	client.CloseIdleConnections()
	wait := time.Duration(min(3+retryCount, 8)) * time.Second
	if notify != nil {
		notify(fmt.Sprintf("🔁 Transient %s on %s — rebuilt client, waiting %ds before one last primary attempt.", label, s.Route().Provider, int(wait.Seconds())))
	}
	timer := time.NewTimer(wait)
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	timer.Stop()
	s.rec.index = 0
	s.rec.activated = false
	return true, nil
}

func (s *Session) reinstallFields(ctx context.Context, st *RouteState) {
	rt := cloneRoute(st.route)
	if s.r.c.o.Providers.IsActualRoute(ctx, rt.Provider, rt.BaseURL) {
		rt.Mode = wire.ModeChat
	}
	s.mu.Lock()
	if cur := s.route; cur != nil {
		rt.Pool = cur.Pool
	}
	s.route = rt
	s.echoOptIn = st.echo
	s.o.Overrides = cloneOverrides(st.overrides)
	s.mu.Unlock()
}
