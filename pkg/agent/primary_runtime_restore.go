package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Turn boundary restore bookkeeping kept between turns
type primaryRestoreState struct {
	restoreWaitLogged    bool
	codexReopenChecked   bool
	codexReopenCheckedAt float64
}

var agentLoadPool = provider.LoadPool

// Earliest instant the pool serves model again, swappable so tests script the reset a pool reports
var restoreNextAvailableAt = func(pool *provider.Pool, model string) (time.Time, bool) { return pool.NextAvailableAt(model) }

// Primary route identity a restore checks pools and pool entries against
type primaryRestoreRoute struct {
	rt                       map[string]any
	provider, model, baseURL string
}

// Whether the pool belongs to the primary, a nil pool matching like an unscoped adapter
func (p primaryRestoreRoute) matchesPool(pool *provider.Pool) bool {
	if pool == nil {
		return true
	}
	return provider.CredentialPoolMatchesProvider(pool.Provider(), p.provider, p.baseURL)
}

// Whether the pool entry belongs to the primary provider and endpoint
func (p primaryRestoreRoute) matchesEntry(entry provider.PooledCredential) bool {
	return provider.CredentialPoolMatchesProvider(entry.Provider, p.provider, p.baseURL)
}

// Primary pool entry scoped for a custom entry, nil when absent or keyed to another provider
func (p primaryRestoreRoute) loadPool(ctx context.Context) *provider.Pool {
	requested := ""
	if v := p.rt["requested_provider"]; v != nil {
		requested = pyrepr.Str(v)
	}
	loaded := provider.LoadRuntimePoolWith(agentPoolEnv(ctx), agentLoadPool, p.provider, p.baseURL, requested, p.rt)
	if loaded != nil && p.matchesPool(loaded) {
		return loaded
	}
	return nil
}

// Puts the agent back on its primary runtime when a fallback ran, true when the restore happened
func (a *AIAgent) restorePrimaryRuntime(ctx context.Context) (bool, error) {
	log := agentRuntimeHelpersLog()
	if !a.fallbackActivated {
		a.fallbackIndex = 0
		return false, a.revertCredentialRotation(ctx)
	}
	rt := a.primaryRuntime
	route := primaryRestoreRoute{rt: rt,
		provider: pystr.Lower(pystr.Strip(pyrepr.Str(orEmpty(rt["provider"])))),
		model:    pystr.Strip(pyrepr.Str(orEmpty(rt["model"]))),
		baseURL:  pyrepr.Str(orEmpty(rt["base_url"]))}
	if route.model != "" && (isEntitlementRejected(a, route.provider, route.model) || provider.IsKnownNonChatModel(route.model)) {
		return false, nil
	}
	if a.primaryQuotaReopenedEarly(ctx, route) {
		a.rateLimitedUntil = 0
	}
	if a.rateLimitedUntil > pythread.Monotonic() {
		return false, nil
	}
	blocked, prefetchedPool, prefetched := a.primaryResetGateBlocks(ctx, route)
	if blocked {
		return false, nil
	}
	a.restoreWaitLogged = false
	fallbackRoute := a.providerFallbackRoute
	if fallbackRoute == ([2]string{}) {
		fallbackRoute = [2]string{a.model, a.provider}
	}
	previousModel, previousProvider := pyStrOr(fallbackRoute[0], "unknown"), pyStrOr(fallbackRoute[1], "unknown")
	providerFallbackActive := a.providerFallbackActive
	if err := guardPhase(func() error { return reinstallPrimaryRuntime(ctx, a, route, prefetchedPool, prefetched) }); err != nil {
		log.Warn(fmt.Sprintf("Failed to restore primary runtime: %s", err.Error()))
		return false, nil
	}
	log.Info(fmt.Sprintf("Primary runtime restored for new turn: %s (%s)", a.model, a.provider))
	a.providerFallbackActive = false
	a.providerFallbackRoute = [2]string{}
	if providerFallbackActive {
		_ = guardPhase(func() error {
			a.emitDiagnosticStatus(fmt.Sprintf("✅ Primary model restored: %s via %s; fallback %s via %s is no longer active.",
				a.model, a.provider, previousModel, previousProvider))
			return nil
		})
	}
	return true, nil
}

// Puts the agent on the primary snapshot, rebinds the primary pool and clears the fallback walk state
func reinstallPrimaryRuntime(ctx context.Context, a *AIAgent, route primaryRestoreRoute, prefetchedPool *provider.Pool, prefetched bool) error {
	if err := reinstallRuntimeSnapshot(ctx, a, route.rt); err != nil {
		return err
	}
	if err := a.rebindPrimaryCredentialPool(ctx, route, prefetchedPool, prefetched); err != nil {
		return err
	}
	a.fallbackActivated = false
	a.fallbackIndex = 0
	a.rateLimitBackoffCount = 0
	return nil
}

// Reloads the primary pool over a cross provider one and reselects its best entry with a usable key
func (a *AIAgent) rebindPrimaryCredentialPool(ctx context.Context, route primaryRestoreRoute, prefetchedPool *provider.Pool, prefetched bool) error {
	log := agentRuntimeHelpersLog()
	if pool := a.credentialPool; pool != nil && pystr.Lower(pystr.Strip(pool.Provider())) != "" && !route.matchesPool(pool) {
		a.credentialPool, a.credentialPoolEntryID = nil, ""
		if prefetched {
			a.credentialPool = prefetchedPool
		} else if err := guardPhase(func() error { a.credentialPool = route.loadPool(ctx); return nil }); err != nil {
			log.Warn(fmt.Sprintf("Restore could not reload primary credential pool for %s: %s", route.provider, err.Error()))
		}
	}
	a.credentialPoolEntryID = ""
	pool := a.credentialPool
	if pool == nil || !pool.HasAvailable(route.model) {
		return nil
	}
	entry, ok := pool.Select(route.model)
	if !ok || (entry.RuntimeAPIKey() == "" && entry.AccessToken == "") {
		return nil
	}
	if !route.matchesEntry(entry) {
		log.Info(fmt.Sprintf("Restore skipped pool entry %s (%s): provider %s does not match primary provider %s",
			entry.ID, entry.Label, pyStrOr(pystr.Lower(pystr.Strip(entry.Provider)), "?"), pyStrOr(route.provider, "?")))
		return nil
	}
	if _, err := swapPoolCredential(ctx, a, entry); err != nil {
		return err
	}
	log.Info(fmt.Sprintf("Restore re-selected pool entry %s (%s)", entry.ID, entry.Label))
	return nil
}

// Moves a live session back onto the credential a quota bench rotated it off once that bench lifts
func (a *AIAgent) revertCredentialRotation(ctx context.Context) error {
	revertID := a.credentialPoolRevertID
	if revertID == "" {
		return nil
	}
	pool := a.credentialPool
	if pool == nil || a.credentialPoolEntryID == revertID {
		a.credentialPoolRevertID = ""
		return nil
	}
	var entry provider.PooledCredential
	var ok bool
	if err := guardPhase(func() error { entry, ok = pool.Reclaim(revertID, a.model); return nil }); err != nil {
		agentRuntimeHelpersLog().Warn(fmt.Sprintf("Credential revert check failed: %s", err.Error()))
		return nil
	}
	if !ok {
		return nil
	}
	swapped, err := swapPoolCredential(ctx, a, entry)
	if err != nil {
		return err
	}
	if swapped {
		agentRuntimeHelpersLog().Info(fmt.Sprintf("Credential %s (%s) available again — reverted pool rotation", entry.ID, entry.Label))
	}
	a.credentialPoolRevertID = ""
	return nil
}

// True when a Codex quota window the primary pool still benches has reopened, checked once per probe interval
func (a *AIAgent) primaryQuotaReopenedEarly(ctx context.Context, route primaryRestoreRoute) (reopened bool) {
	if route.provider != "openai-codex" {
		return false
	}
	now := pythread.Monotonic()
	if a.codexReopenChecked && now-a.codexReopenCheckedAt < provider.CodexQuotaProbeMinInterval.Seconds() {
		return false
	}
	a.codexReopenChecked, a.codexReopenCheckedAt = true, now
	if err := guardPhase(func() error {
		pool := a.credentialPool
		if pool == nil || !route.matchesPool(pool) {
			pool = route.loadPool(ctx)
		}
		if pool == nil {
			return nil
		}
		benchedUntil, ok := restoreNextAvailableAt(pool, route.model)
		if !ok || !benchedUntil.After(time.Now()) {
			return nil
		}
		reopened = pool.LiftReopenedCooldowns(route.model)
		return nil
	}); err != nil {
		agentRuntimeHelpersLog().Debug("Early quota-reopen check failed; keeping the cooldown")
		return false
	}
	return reopened
}

// Blocks a restore while the primary pool reports a future reset, returning the pool it had to load
func (a *AIAgent) primaryResetGateBlocks(ctx context.Context, route primaryRestoreRoute) (blocked bool, prefetchedPool *provider.Pool, prefetched bool) {
	if err := guardPhase(func() error {
		pool := a.credentialPool
		if !route.matchesPool(pool) {
			pool = route.loadPool(ctx)
			prefetchedPool, prefetched = pool, true
		}
		if route.rt == nil {
			return fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'get'")
		}
		if pool == nil {
			return nil
		}
		nextAt, ok := restoreNextAvailableAt(pool, route.model)
		if !ok || !nextAt.After(time.Now()) {
			return nil
		}
		if !a.restoreWaitLogged {
			a.restoreWaitLogged = true
			agentRuntimeHelpersLog().Info(fmt.Sprintf("Primary %s rate-limited until %s; staying on fallback %s/%s until the reset elapses",
				pyStrOr(route.provider, "?"), nextAt.Local().Format("2006-01-02T15:04:05"), a.provider, a.model))
		}
		blocked = true
		return nil
	}); err != nil {
		agentRuntimeHelpersLog().Debug("Reset-aware restore gate failed; falling back to per-turn retry")
		return false, prefetchedPool, prefetched
	}
	return blocked, prefetchedPool, prefetched
}
