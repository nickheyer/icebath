package model

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	authEpisodeIdle        = 120 * time.Second
	authActivityTouch      = 30 * time.Second
	maxAuthRefreshAttempts = 2
)

type authEpisode struct {
	refreshes int
	waits     int
	last      time.Time
}

func transientAuthWait(step int) (time.Duration, int, bool) {
	switch step {
	case 0:
		return 5 * time.Second, 3, true
	case 1:
		return 15 * time.Second, 3, true
	case 2:
		return 30 * time.Second, 3, true
	}
	return 0, 3, false
}

type PoolRecovery struct {
	Status            int
	Retried429        bool
	Reason            Class
	Classified        bool
	Context           ErrorContext
	UsageWindow       bool
	UpstreamProvider  string
	BillingUnverified bool
	Notify            func(text string)
	Activity          func(text string)
}

func (s *Session) episode(provider, entryID string) *authEpisode {
	now := time.Now()
	k := [2]string{provider, entryID}
	ep := s.rec.episodes[k]
	if ep == nil || now.Sub(ep.last) > authEpisodeIdle {
		ep = &authEpisode{}
		s.rec.episodes[k] = ep
	}
	ep.last = now
	return ep
}

func (s *Session) PortalRefreshedThisTurn() bool {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return s.portalRefreshedLocked()
}

func (s *Session) portalRefreshedLocked() bool {
	for k, ep := range s.rec.episodes {
		if k[0] == portal.Provider && ep.refreshes > 0 {
			return true
		}
	}
	return false
}

func (s *Session) interruptibleWait(ctx context.Context, d time.Duration, activity func(string)) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	touch := time.NewTicker(authActivityTouch)
	defer touch.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-touch.C:
			if activity != nil {
				activity("waiting out an Icebath Portal 401 on a valid key")
			}
		}
	}
}

func entryFor(pool *auth.Pool, id string, hint auth.Secret) (auth.Entry, bool) {
	entries := pool.Entries()
	if id != "" {
		for _, e := range entries {
			if e.ID == id {
				return e, true
			}
		}
	}
	if !hint.Empty() {
		for _, e := range entries {
			if e.RuntimeAPIKey().Reveal() == hint.Reveal() {
				return e, true
			}
		}
	}
	return auth.Entry{}, false
}

func (s *Session) swapCredential(ctx context.Context, e auth.Entry) bool {
	rt := s.Route()
	key := e.RuntimeAPIKey()
	if key.Empty() {
		key = e.AccessToken
	}
	base := firstOf(e.RuntimeBaseURL(ctx), e.BaseURL, rt.BaseURL)
	actual := s.r.c.o.Providers.IsActualRoute(ctx, rt.Provider, base)
	if actual {
		base = auth.NormalizeActualBaseURL(base)
	}
	stripped := strings.TrimRight(base, "/")
	if !portal.RouteCanServeModel(rt.Provider, stripped, rt.Model) {
		slog.Info(fmt.Sprintf("Credential %s skipped: its route cannot serve model %s", firstOf(e.ID, "?"), rt.Model))
		return false
	}
	n := cloneRoute(rt)
	if actual {
		n.Mode = wire.ModeChat
	}
	s.rec.entryID = e.ID
	n.Credential = staticKey(key.Reveal(), e.Source)
	n.BaseURL = stripped
	s.setRoute(n)
	return true
}

func (s *Session) failedCredential(pool *auth.Pool) (auth.Secret, string) {
	rt := s.Route()
	var hint auth.Secret
	if rt.Credential.Kind == auth.CredAPIKey || rt.Credential.Kind == auth.CredXAPIKey {
		hint = rt.Credential.Key
	}
	id := s.rec.entryID
	if hint.Empty() {
		if cur, ok := pool.Current(); ok {
			hint = cur.RuntimeAPIKey()
			if id == "" {
				id = cur.ID
			}
		}
	}
	return hint, id
}

func (s *Session) entitlement403(status int, c ErrorContext) bool {
	if EntitlementFailure(c, status) {
		return true
	}
	if status != 403 {
		return false
	}
	haystack := strings.ToLower(c.Message) + " " + strings.ToLower(c.Reason)
	if strings.Contains(haystack, "oauth authentication is currently not allowed for this organization") {
		return true
	}
	rt := s.Route()
	if rt.Provider == "anthropic" && rt.Mode == wire.ModeAnthropic {
		return true
	}
	if rt.Provider == "xai-oauth" {
		return !(strings.Contains(haystack, "[wke=unauthenticated:") || strings.Contains(haystack, "oauth2 access token could not be validated"))
	}
	return false
}

func (s *Session) RecoverWithPool(ctx context.Context, in PoolRecovery) (recovered, retried429 bool, err error) {
	ctx = s.r.bind(ctx)
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	rt := s.Route()
	pool := rt.Pool
	if pool == nil {
		return false, in.Retried429, nil
	}
	current := strings.ToLower(strings.TrimSpace(rt.Provider))
	if pp := strings.ToLower(strings.TrimSpace(pool.Provider())); pp != "" && !s.r.c.o.Auth.PoolMatchesProvider(ctx, pool.Provider(), current, rt.BaseURL) {
		slog.Warn(fmt.Sprintf("Credential pool provider mismatch: pool=%s, agent=%s — skipping pool mutation to avoid cross-provider contamination", pp, current))
		return false, in.Retried429, nil
	}
	hint, id := s.failedCredential(pool)
	reason, classified := in.Reason, in.Classified
	if !classified {
		switch in.Status {
		case 402:
			reason, classified = ClassBilling, true
		case 429:
			reason, classified = ClassRateLimit, true
		case 401, 403:
			reason, classified = ClassAuth, true
		}
	}
	rotate := func(defaultStatus int, label string) (bool, error) {
		status := in.Status
		if status == 0 {
			status = defaultStatus
		}
		f := auth.Failure{Status: status, ErrorReason: in.Context.Reason, Message: in.Context.Message, ResetAt: in.Context.ResetAt,
			APIKeyHint: hint, CredentialID: id}
		if classified {
			f.Reason = auth.PoolFailureReason(reason.String(), in.UsageWindow, in.BillingUnverified)
		}
		if strings.TrimSpace(rt.Model) != "" {
			f.Model = rt.Model
		}
		next, ok, err := pool.MarkExhausted(ctx, f)
		if err != nil || !ok {
			return false, err
		}
		if !auth.EntryServesEndpoint(ctx, next, rt.BaseURL, catalog.NormalizeRouteBaseURL) {
			slog.Info(fmt.Sprintf("Credential %d (%s) — pool entry %s serves another endpoint; not swapping", status, label, firstOf(next.ID, "?")))
			return false, nil
		}
		slog.Info(fmt.Sprintf("Credential %d (%s) — rotated to pool entry %s", status, label, firstOf(next.ID, "?")))
		swapped := s.swapCredential(ctx, next)
		if swapped && id != "" && s.rec.revertID == "" && classified && (reason == ClassRateLimit || reason == ClassBilling) {
			if benched, ok := entryFor(pool, id, auth.Secret{}); ok && benched.ID == id && benched.Priority < next.Priority {
				s.rec.revertID = id
			}
		}
		return swapped, nil
	}
	outcome := func(ok bool, err error, onFail bool) (bool, bool, error) {
		if err != nil {
			return false, in.Retried429, err
		}
		if ok {
			return true, false, nil
		}
		return false, onFail, nil
	}
	if !classified {
		return false, in.Retried429, nil
	}
	switch reason {
	case ClassUpstreamRateLimit:
		if in.UpstreamProvider != "" {
			slog.Info(fmt.Sprintf("Upstream provider %s rate-limited via aggregator — skipping credential rotation, deferring to fallback chain", in.UpstreamProvider))
		} else {
			slog.Info("Upstream aggregator 429 (provider unknown) — skipping credential rotation, deferring to fallback chain")
		}
		return false, in.Retried429, nil
	case ClassBilling:
		ok, err := rotate(402, "billing")
		return outcome(ok, err, in.Retried429)
	case ClassRateLimit:
		return s.recoverRateLimit(pool, in, hint, id, rotate, outcome)
	case ClassModelEntitlement:
		ok, err := rotate(400, "model entitlement")
		if err != nil {
			return false, in.Retried429, err
		}
		return ok, in.Retried429, nil
	case ClassAuth:
		return s.recoverAuthFailure(ctx, pool, in, hint, id, rotate)
	}
	return false, in.Retried429, nil
}

func (s *Session) recoverRateLimit(pool *auth.Pool, in PoolRecovery, hint auth.Secret, id string,
	rotate func(int, string) (bool, error), outcome func(bool, error, bool) (bool, bool, error)) (bool, bool, error) {
	cur, found := entryFor(pool, id, hint)
	if !found {
		cur, found = pool.Current()
	}
	if found && cur.LastStatus == auth.StatusExhausted {
		slog.Info(fmt.Sprintf("Credential already exhausted (last_status=%s) — rotating immediately instead of retrying", cur.LastStatus))
		ok, err := rotate(429, "rate limit, pre-exhausted")
		return outcome(ok, err, true)
	}
	reason, message := strings.ToLower(in.Context.Reason), strings.ToLower(in.Context.Message)
	usageLimit := strings.Contains(reason, "usage_limit_reached") || strings.Contains(reason, "gousagelimit") ||
		strings.Contains(message, "usage limit reached") || strings.Contains(message, "usage limit has been reached")
	if !in.Retried429 && !usageLimit {
		return false, true, nil
	}
	ok, err := rotate(429, "rate limit")
	return outcome(ok, err, true)
}

func (s *Session) recoverAuthFailure(ctx context.Context, pool *auth.Pool, in PoolRecovery, hint auth.Secret, id string,
	rotate func(int, string) (bool, error)) (bool, bool, error) {
	if s.entitlement403(in.Status, in.Context) {
		label := "auth"
		if in.Status != 0 {
			label = fmt.Sprint(in.Status)
		}
		slog.Info(fmt.Sprintf("Credential %s — entitlement-shaped 403 from %s; skipping pool refresh (account lacks subscription, not a transient auth failure).",
			label, firstOf(s.Route().Provider, "provider")))
		return false, in.Retried429, nil
	}
	provider := s.Route().Provider
	portal401 := provider == portal.Provider && in.Status == 401
	entryID := id
	if portal401 {
		if found, ok := entryFor(pool, id, hint); ok {
			entryID = firstOf(found.ID, id)
		}
		if ep := s.episode(provider, entryID); ep.refreshes >= 1 {
			return s.retryAfterBackoff(ctx, pool, ep, hint, entryID, id, in)
		}
	}
	refreshed, ok, err := pool.TryRefreshMatching(ctx, hint, id)
	if err != nil {
		return false, in.Retried429, err
	}
	if !ok {
		swapped, err := rotate(401, "auth refresh failed")
		if err != nil {
			return false, in.Retried429, err
		}
		if swapped {
			return true, false, nil
		}
		return false, in.Retried429, nil
	}
	if portal401 {
		ep := s.episode(provider, firstOf(refreshed.ID, entryID))
		ep.refreshes++
		if !hint.Empty() && refreshed.RuntimeAPIKey().Reveal() == hint.Reveal() {
			return s.retryAfterBackoff(ctx, pool, ep, hint, refreshed.ID, id, in)
		}
	} else if refreshed.ID != "" {
		ep := s.episode(provider, refreshed.ID)
		ep.refreshes++
		if ep.refreshes > maxAuthRefreshAttempts {
			slog.Warn(fmt.Sprintf("Credential auth failure persists after %d refreshes for pool entry %s — treating as unrecoverable and allowing fallback to activate.",
				ep.refreshes-1, refreshed.ID))
			return false, in.Retried429, nil
		}
	}
	slog.Info("Credential auth failure — refreshed pool entry " + firstOf(refreshed.ID, "?"))
	if !s.swapCredential(ctx, refreshed) {
		return false, in.Retried429, nil
	}
	return true, in.Retried429, nil
}

func (s *Session) retryAfterBackoff(ctx context.Context, pool *auth.Pool, ep *authEpisode, failed auth.Secret, entryID, credentialID string,
	in PoolRecovery) (bool, bool, error) {
	step := ep.waits
	wait, total, ok := transientAuthWait(step)
	if !ok {
		slog.Warn(fmt.Sprintf("Credential auth failure persists for pool entry %s after one refresh and %d backoff retries — treating as unrecoverable and allowing fallback to activate.",
			noneText(entryID), step))
		return false, in.Retried429, nil
	}
	ep.waits = step + 1
	slog.Info(fmt.Sprintf("Credential auth failure on an unexpired key for pool entry %s — transient 401, retrying in %.0fs (backoff %d/%d, no further refresh)",
		noneText(entryID), wait.Seconds(), step+1, total))
	if in.Notify != nil {
		in.Notify(fmt.Sprintf("🔐 Icebath Portal rejected a still-valid key (transient); retrying in %.0fs...", wait.Seconds()))
	}
	if !s.interruptibleWait(ctx, wait, in.Activity) {
		return false, in.Retried429, nil
	}
	if _, found := entryFor(pool, firstOf(credentialID, entryID), failed); found {
		fresh, err := s.r.c.o.Auth.Pool(ctx, pool.Provider())
		if err != nil {
			return false, in.Retried429, err
		}
		synced, ok := entryFor(fresh, firstOf(credentialID, entryID), failed)
		if !ok {
			return false, in.Retried429, nil
		}
		rt := cloneRoute(s.Route())
		rt.Pool = fresh
		s.setRoute(rt)
		if !s.swapCredential(ctx, synced) {
			return false, in.Retried429, nil
		}
	}
	return true, in.Retried429, nil
}

func noneText(v string) string {
	if v == "" {
		return "None"
	}
	return v
}
