package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
)

// Sets gateway.multiplex_profiles in the default profile's config through the guarded config write
func writeMultiplexFlag(ctx context.Context, defaultHome string, value bool) error {
	cfgPath := filepath.Join(defaultHome, "config.yaml")
	userConfig, err := config.RequireReadableConfigBeforeWrite(ctx, cfgPath)
	if err != nil {
		return err
	}
	delete(userConfig, "multiplex_profiles")
	if err := config.Tree(userConfig).Set("gateway.multiplex_profiles", value); err != nil {
		return err
	}
	return config.WriteUserConfig(ctx, defaultHome, cfgPath, userConfig)
}
