package config

import (
	"context"
	"errors"
	"log/slog"

	"github.com/nickheyer/icebath/internal/config/remote"
)

// Settings of a profile Remote Config holds, every name as the plane spells it
type remoteLayer struct {
	profile *remote.Profile
}

func (r *remoteLayer) get(key keyPath) any {
	value, _ := lookup(r.profile.Doc(), key)
	return value
}

func (r *remoteLayer) holds(key keyPath) bool {
	_, ok := lookup(r.profile.Doc(), key)
	return ok
}

func (r *remoteLayer) all() map[string]any {
	return r.profile.Doc()
}

// Names of the mapping under the key in order of their names, as the plane keeps no order of its own
func (r *remoteLayer) order(key keyPath) []string {
	value, _ := lookup(r.profile.Doc(), key)
	m, _ := value.(map[string]any)
	return ordered(m, nil)
}

// Fetches the profile's settings again when the plane changed them; a failure keeps those loaded and is told, as a poll's is
func (r *remoteLayer) load() error {
	if err := r.profile.Refresh(context.Background()); err != nil {
		slog.Warn("Remote Config: refresh failed; keeping the loaded config", "error", err)
	}
	return nil
}

// Saves the key at the profile's level, each part that keeps the spelling held spelled as the plane holds it, a lock of a level above it
// refused as any locked key is
func (r *remoteLayer) set(key keyPath, value any) error {
	err := r.profile.Set(context.Background(), remote.Encode(resolve(r.profile.Doc(), key).parts), value)
	if locked, ok := errors.AsType[*remote.LockedError](err); ok {
		return &LockedError{Key: key.String(), By: ByRemote, Source: locked.Level}
	}
	return err
}

func (r *remoteLayer) where() string   { return r.profile.Destination() }
func (r *remoteLayer) writable() error { return nil }

// Level that locks the key or a key above it, each part of a lock matched as the key answers to names, so a lock written in another case
// than the key icebath declares still holds
func (r *remoteLayer) locked(key keyPath) string {
	for _, lock := range r.profile.Locks() {
		if covers(lock.Path, key) {
			return lock.Level
		}
	}
	return ""
}

// Whether the lock covers the key: its parts start the key's
func covers(lock []string, key keyPath) bool {
	if len(lock) > len(key.parts) {
		return false
	}
	for i, part := range lock {
		if !same(part, key.parts[i], key.match[i]) {
			return false
		}
	}
	return true
}
