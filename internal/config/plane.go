package config

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/config/remote"
)

// Settings of a profile Remote Config holds
type remoteLayer struct {
	profile *remote.Profile
}

// Value of a document under the dotted key, its keys matched in any case as every config.yaml's are, false when it holds none
func lookupFolded(doc map[string]any, key string) (any, bool) {
	var node any = doc
	for _, part := range strings.Split(key, ".") {
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		found := false
		for name, value := range m {
			if strings.EqualFold(name, part) {
				node, found = value, true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return node, true
}

// Document with its keys folded to lower case, as viper folds those of a config.yaml
func folded(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[strings.ToLower(key)] = folded(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = folded(item)
		}
		return out
	}
	return value
}

func (r *remoteLayer) get(key string) any {
	value, _ := lookupFolded(r.profile.Doc(), key)
	return folded(value)
}

func (r *remoteLayer) holds(key string) bool {
	_, ok := lookupFolded(r.profile.Doc(), key)
	return ok
}

func (r *remoteLayer) all() map[string]any {
	doc, _ := folded(r.profile.Doc()).(map[string]any)
	return doc
}

// Fetches the profile's settings again when the plane changed them; a failure keeps those loaded and is told, as a poll's is
func (r *remoteLayer) load() error {
	if err := r.profile.Refresh(context.Background()); err != nil {
		slog.Warn("Remote Config: refresh failed; keeping the loaded config", "error", err)
	}
	return nil
}

// Saves the key at the profile's level, a lock of a level above it refused as any locked key is
func (r *remoteLayer) set(key string, value any) error {
	err := r.profile.Set(context.Background(), key, value)
	if locked, ok := errors.AsType[*remote.LockedError](err); ok {
		return &LockedError{Key: key, By: ByRemote, Source: locked.Level}
	}
	return err
}

func (r *remoteLayer) where() string   { return r.profile.Destination() }
func (r *remoteLayer) writable() error { return nil }

func (r *remoteLayer) locked(key string) string {
	level, _ := r.profile.Locked(key)
	return level
}
