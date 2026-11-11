package tuigateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// config.yaml of the per session profile override when bound, else the launch home
func activeConfigPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "config.yaml")
}

// Effective user config of the active profile without the defaults merge, empty on any failure
func loadCfg(ctx context.Context) (cfg map[string]any) {
	defer func() {
		if recover() != nil {
			cfg = map[string]any{}
		}
	}()
	loaded, err := config.LoadUserEffective(activeConfigPath(ctx), config.EnvRefLookupContext(ctx))
	if err != nil || loaded == nil {
		return map[string]any{}
	}
	return loaded
}
