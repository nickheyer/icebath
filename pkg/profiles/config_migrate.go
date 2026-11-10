package profiles

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Migrates a copied config.yaml of a new profile to the current schema, never failing creation over the migration
func MigrateProfileConfigIfOutdated(ctx context.Context, profileDir string) error {
	ok, err := config.SupportsFileTooling(ctx)
	if err != nil || !ok {
		return err
	}
	exists, err := config.ConfigExists(ctx, filepath.Join(profileDir, "config.yaml"))
	if err != nil || !exists {
		return err
	}
	scoped := home.WithOverride(ctx, profileDir)
	current, latest, err := config.CheckConfigVersion(ctx, profileDir, false)
	if err == nil && current < latest {
		_, _ = config.MigrateConfig(scoped, profileDir, false, true)
	}
	return nil
}
