package gateway

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Commits and drops the cached agent of a key no conversation holds, forgetting its ids and source
func forgetDetached(ctx context.Context, a *SessionAuthority, key string) error {
	if evict, ok := a.Runner.(boundaryEvictingRunner); ok {
		evict.EvictCachedAgentAtBoundary(key)
	}
	dropOwnerHints(a.Runner.SessionStore(), key)
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return err
	}
	held, _ := adapters.Get(PlatformLocal)
	if local, ok := held.(*LocalSessionAdapter); ok && local != nil {
		local.mu.Lock()
		delete(local.sources, key)
		local.mu.Unlock()
	}
	return nil
}

// Key a session turn runs under, its detached key while a turn started there runs, else the route
func runningKey(runner AuthorityRunner, route, sessionID string) string {
	key := detachedKey(route, sessionID)
	if running, ok := runner.(runningAgentsRunner); ok {
		if agent, pending := running.RunningAgent(key); agent != nil || pending {
			return key
		}
	}
	return route
}

// Authority owning a route, the launch profile's for its own keys and the routed home's for a named profile key
func routeAuthority(runner AuthorityRunner, store *SessionStore, route string) (*SessionAuthority, error) {
	registry := registryOf(runner)
	if store.namedProfileForKey(route) == "" {
		if registry == nil {
			if bare, ok := runner.(launchAuthorityRunner); ok {
				return bare.SessionAuthority(), nil
			}
			return nil, nil
		}
		for _, authority := range registry.Serving() {
			if home.Key(authority.ProfileID) == registry.LaunchKey() {
				return authority, nil
			}
		}
		return nil, nil
	}
	profileHome := store.profileHomeForKey(route)
	if profileHome == "" {
		return nil, nil
	}
	return authorityForHome(runner, profileHome)
}

// Authority serving a home, the bare runner's launch authority only when its store lives in that home
func authorityForHome(runner any, profileHome string) (*SessionAuthority, error) {
	registry := registryOf(runner)
	if registry != nil {
		return registry.ForHome(profileHome), nil
	}
	bare, ok := runner.(launchAuthorityRunner)
	if !ok {
		return nil, nil
	}
	authority := bare.SessionAuthority()
	if authority == nil {
		return nil, nil
	}
	dbPath, err := pypath.Resolve(authority.DB.Path())
	if err != nil {
		return nil, err
	}
	wanted, err := pypath.Resolve(profileHome)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(dbPath) == wanted {
		return authority, nil
	}
	return nil, nil
}

// Wires the store's route announcements to the owning authority's lane naming, a stale announcement ignored
func watchRoutes(ctx context.Context, runner AuthorityRunner, store *SessionStore) {
	scope := context.WithoutCancel(ctx)
	store.RouteNamed = func(route, sessionID string, origin *SessionSource) {
		authority, err := routeAuthority(runner, store, route)
		if err == nil && (authority == nil || origin == nil) {
			return
		}
		if err == nil {
			err = nameAnnouncedLane(scope, authority, store, route, sessionID, origin)
		}
		if err != nil {
			slog.Error("Exception in callback watch_routes.<locals>.route_named.<locals>.name(): " + pyExceptionRepr(err))
		}
	}
}

// Names the announced conversation on its route unless the store has since moved past it
func nameAnnouncedLane(ctx context.Context, a *SessionAuthority, store *SessionStore, route, sessionID string, origin *SessionSource) error {
	current := store.LookupBySessionKey(ctx, route)
	owner, err := a.LogicalOwner(ctx, sessionID)
	if err != nil || current == nil {
		return err
	}
	currentOwner, err := a.LogicalOwner(ctx, current.SessionID)
	if err != nil || currentOwner != owner {
		return err
	}
	return a.nameLane(ctx, route, owner, origin)
}
