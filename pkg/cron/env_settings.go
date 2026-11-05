package cron

import (
	"context"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Tuning setting from the active secret scope or this profile env file under multiplex, else the process env
func (s *Store) cronEnvSetting(ctx context.Context, name, def string) string {
	if !provider.IsMultiplexActive(ctx) {
		if v := os.Getenv(name); v != "" {
			return v
		}
		return def
	}
	scope := provider.CurrentSecretScope(ctx)
	if scope == nil {
		scope = config.LoadEnvFile(filepath.Join(s.home, ".env"))
	}
	value, ok := scope[name]
	if !ok {
		return def
	}
	return value
}
