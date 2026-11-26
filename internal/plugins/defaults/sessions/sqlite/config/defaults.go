package config

import (
	"path/filepath"

	"github.com/nickheyer/icebath/internal/config"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: sessionsconfig.Key,
		Title:  "SQLite",
		About:  "Sessions saved in a local database file",
		Settings: []config.Setting{
			{Key: Path, Title: "File", About: "Database file path", Default: filepath.Join(cfg.Dir(), "sessions.db")},
		},
	}})
}
