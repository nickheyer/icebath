package sessions

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/checkpoints"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/sessions"
)

// Cleans up old snapshots of every profile as a long-lived server starts, and forgets what was snapshotted for a session once it is done
func Register(registry *sessions.Registry, cfg *config.Config, forget func(session string)) {
	registry.OnStart(func(context.Context) { prune(cfg) })
	registry.OnRelease(func(_ context.Context, session string) { forget(session) })
}

// Runs the automatic cleanup of each profile as its settings have it
func prune(cfg *config.Config) {
	entries, err := os.ReadDir(filepath.Dir(cfg.ProfileDir(profile.Default)))
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !config.Named(entry.Name()) {
			continue
		}
		if _, _, err := checkpoints.AutoPrune(cfg.In(entry.Name())); err != nil {
			slog.Warn("checkpoint auto-maintenance failed", "profile", entry.Name(), "error", err)
		}
	}
}
