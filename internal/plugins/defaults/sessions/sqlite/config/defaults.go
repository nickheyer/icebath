package config

import (
	"path/filepath"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config) {
	cfg.SetDefault(Path, filepath.Join(cfg.Dir(), "sessions.db"))
}
