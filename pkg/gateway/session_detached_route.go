package gateway

import "context"

// Session and live entry of the conversation still live on a detached key, false for any other route
func detachedLive(a *SessionAuthority, route string) (SessionRef, *LiveSession, bool) {
	sessionID, ok := detachedSessionID(route)
	if !ok || sessionID == "" {
		return SessionRef{}, nil, false
	}
	live, ok := a.live(sessionID)
	if !ok || live.Route != route {
		return SessionRef{}, nil, false
	}
	return SessionRef{a.ProfileID, sessionID}, live, true
}

// Routing entry a route names, the store's or a detached key's live conversation
func routeEntry(ctx context.Context, a *SessionAuthority, route string) (*SessionEntry, error) {
	store := a.Runner.SessionStore()
	if entry := store.LookupBySessionKey(ctx, route); entry != nil {
		return entry, nil
	}
	if _, live, found := detachedLive(a, route); found {
		return detachedEntry(ctx, store, live.Source)
	}
	return nil, nil
}
