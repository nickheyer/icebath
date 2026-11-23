package tuihost

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session settings kept in the row of each session in its home's state.db, so they survive a restart and a resume and go with the session
type sessionSettingsStore struct{}

// RegisterSessionSettingsStore makes the session rows the store of every session's own settings in this process
func RegisterSessionSettingsStore() { settings.RegisterSessionStore(sessionSettingsStore{}) }

// Store and row of a session's settings: a live record's stored row, else the row of that id in the home ctx carries, with the release of the store
func sessionSettingsRow(ctx context.Context, sessionID string) (*state.DB, string, func(), error) {
	rowID, h := sessionID, home.Dir(ctx)
	if record := SessionRecord(sessionID); record != nil {
		if key, _ := record.Get("session_key").(string); strings.TrimSpace(key) != "" {
			rowID = key
		}
		if profileHome, _ := record.Get("profile_home").(string); profileHome != "" {
			h = profileHome
		}
	}
	db, err := state.Acquire(ctx, home.StatePath(h))
	if err != nil {
		return nil, "", func() {}, err
	}
	return db, rowID, func() { state.Release(db) }, nil
}

// LoadSessionSettings reads the values a session stored for itself, empty when it stored none
func (sessionSettingsStore) LoadSessionSettings(ctx context.Context, sessionID string) (map[string]any, error) {
	db, rowID, release, err := sessionSettingsRow(ctx, sessionID)
	defer release()
	if err != nil {
		return nil, err
	}
	cfg, err := db.SessionModelConfig(ctx, rowID)
	if err != nil {
		return nil, err
	}
	values, _ := cfg[state.SessionSettingsKey].(map[string]any)
	return values, nil
}

// SaveSessionSettings replaces the values a session stores for itself, an empty map removing them
func (sessionSettingsStore) SaveSessionSettings(ctx context.Context, sessionID string, values map[string]any) error {
	db, rowID, release, err := sessionSettingsRow(ctx, sessionID)
	defer release()
	if err != nil {
		return err
	}
	return db.UpdateSessionModelConfig(ctx, rowID, func(cfg map[string]any) bool {
		if len(values) == 0 {
			_, held := cfg[state.SessionSettingsKey]
			delete(cfg, state.SessionSettingsKey)
			return held
		}
		cfg[state.SessionSettingsKey] = values
		return true
	})
}
