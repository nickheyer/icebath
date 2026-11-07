package gateway

import (
	"context"
	"strings"
	"time"
)

const detachedSlot = "detached"

// Pinned detached route key of the source, nil for any other source
func detachedRouteOf(source *SessionSource) *string {
	if source == nil {
		return nil
	}
	return source.DetachedRoute
}

// Detached key of the session id in the profile namespace of its chat route
func detachedKey(route, sessionID string) string {
	parts := strings.Split(route, ":")
	namespace := "agent:main"
	if len(parts) > 1 && parts[0] == "agent" {
		namespace = strings.Join(parts[:2], ":")
	}
	return namespace + ":" + detachedSlot + ":" + sessionID
}

// Session id a detached key names, false for any other key
func detachedSessionID(key string) (string, bool) {
	parts := strings.Split(key, ":")
	if len(parts) == 4 && parts[0] == "agent" && parts[2] == detachedSlot {
		return parts[3], true
	}
	return "", false
}

// Source as its chat's own with a detached copy's pin dropped
func laneSource(source *SessionSource) *SessionSource {
	if detachedRouteOf(source) == nil {
		return source
	}
	return ReplaceSource(source, func(s *SessionSource) { s.DetachedRoute = nil })
}

// Forgets every session id hint the key owned
func dropOwnerHints(store *SessionStore, key string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for sid, owner := range store.sessionOwnerHints {
		if owner == key {
			delete(store.sessionOwnerHints, sid)
		}
	}
}

// True when the key is the session id's own detached key
func ownDetachedKey(key any, sessionID string) bool {
	k, ok := key.(string)
	if !ok {
		return false
	}
	sid, found := detachedSessionID(k)
	return found && sid == sessionID
}

// Never stored routing entry of a detached source at the compression tip its turns write to
func detachedEntry(ctx context.Context, store *SessionStore, source *SessionSource) (*SessionEntry, error) {
	key := *source.DetachedRoute
	sessionID, ok := detachedSessionID(key)
	if !ok {
		sessionID = "None"
	}
	db := store.dbForKey(ctx, key)
	tip := sessionID
	var started float64
	if db != nil {
		row, err := db.GetSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if row != nil {
			if v, _ := row.Get("started_at"); v != nil && v != false {
				f, err := pyFloat(v)
				if err != nil {
					return nil, err
				}
				started = f
			}
		}
		compressed, err := db.GetCompressionTip(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if compressed != "" {
			tip = compressed
		}
	}
	store.mu.Lock()
	store.sessionOwnerHints[sessionID], store.sessionOwnerHints[tip] = key, key
	store.mu.Unlock()
	now := sessionNow()
	createdAt := pyDatetime{T: now.T.Add(-time.Second)}
	if started != 0 {
		createdAt = pyDatetime{T: time.Unix(0, int64(started*1e9))}
	}
	entry := NewSessionEntry(key, tip, createdAt, now)
	entry.Origin, entry.Platform, entry.ChatType = source, source.Platform, source.ChatType
	return entry, nil
}
