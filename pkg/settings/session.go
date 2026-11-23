package settings

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// Keeps the session scope values of each session with the session itself, so they survive a restart and a resume
type SessionStore interface {
	// Values a session stored, empty when it stored none; ctx carries the session's home
	LoadSessionSettings(ctx context.Context, sessionID string) (map[string]any, error)
	// Replaces every value a session stores, an empty map removing them all
	SaveSessionSettings(ctx context.Context, sessionID string, values map[string]any) error
}

// Store the runtime registered, nil keeping session values in this process only
var sessionStore struct {
	sync.RWMutex
	store SessionStore
}

// Registers the store session values live in, replacing any earlier one
func RegisterSessionStore(s SessionStore) {
	sessionStore.Lock()
	sessionStore.store = s
	sessionStore.Unlock()
	sessionLayer.Lock()
	clear(sessionLayer.values)
	sessionLayer.Unlock()
}

// Registered session store, nil when none
func currentSessionStore() SessionStore {
	sessionStore.RLock()
	defer sessionStore.RUnlock()
	return sessionStore.store
}

// Values of the sessions this process has read or written, keyed by session then setting
var sessionLayer = struct {
	sync.Mutex
	values map[string]map[string]any
}{values: map[string]map[string]any{}}

// Values a session holds, read from the store on first use; the map is shared and never mutated
func sessionValues(ctx context.Context, sessionID string) map[string]any {
	if sessionID == "" {
		return nil
	}
	sessionLayer.Lock()
	values, loaded := sessionLayer.values[sessionID]
	sessionLayer.Unlock()
	if loaded {
		return values
	}
	store := currentSessionStore()
	if store == nil {
		return nil
	}
	stored, err := store.LoadSessionSettings(ctx, sessionID)
	if err != nil {
		pkgLog().Warn("settings: reading a session's own settings failed", "session", sessionID, "err", err)
		return nil
	}
	values = map[string]any{}
	for key, v := range stored {
		if e := Find(key); e != nil {
			if coerced, err := e.Coerce(v); err == nil {
				values[e.Info().Key] = coerced
				continue
			}
		}
		pkgLog().Warn("settings: dropping a stored session value", "session", sessionID, "key", key)
	}
	sessionLayer.Lock()
	defer sessionLayer.Unlock()
	if current, raced := sessionLayer.values[sessionID]; raced {
		return current
	}
	sessionLayer.values[sessionID] = values
	return values
}

// Value a session holds for a key
func sessionValue(ctx context.Context, sessionID, key string) (any, bool) {
	v, ok := sessionValues(ctx, sessionID)[key]
	return v, ok
}

// Stores and drops session values in one save, the memory left as it was when saving fails
func storeSessionValues(ctx context.Context, sessionID string, set map[string]any, drop []string) error {
	next := maps.Clone(sessionValues(ctx, sessionID))
	if next == nil {
		next = map[string]any{}
	}
	for _, key := range drop {
		delete(next, key)
	}
	maps.Copy(next, set)
	if store := currentSessionStore(); store != nil {
		if err := store.SaveSessionSettings(ctx, sessionID, maps.Clone(next)); err != nil {
			return fmt.Errorf("settings: saving the session's own settings: %w", err)
		}
	}
	sessionLayer.Lock()
	sessionLayer.values[sessionID] = next
	sessionLayer.Unlock()
	return nil
}

// Every value a session holds, a copy the caller may change
func SessionValues(ctx context.Context, sessionID string) map[string]any {
	return maps.Clone(sessionValues(ctx, sessionID))
}

// Forgets the values this process holds for a session whose live run ended; the store keeps them for a resume
func EndSession(sessionID string) {
	sessionLayer.Lock()
	delete(sessionLayer.values, sessionID)
	sessionLayer.Unlock()
}
