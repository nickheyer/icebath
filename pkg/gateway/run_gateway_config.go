package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Profiles this multiplexer serves, the boot reservation once taken, else the live profiles directory
func multiplexProfileHomes(ctx context.Context, cfg *GatewayConfig) ([]profiles.ServedProfile, error) {
	if cfg != nil && cfg.runtimeProfileHomes != nil {
		return slices.Clone(*cfg.runtimeProfileHomes), nil
	}
	return profiles.ProfilesToServe(ctx, true, false, false)
}

// Effective user config.yaml at a path or the active gateway home, an empty mapping on any failure
func loadGatewayConfig(ctx context.Context, configPath string) (cfg map[string]any) {
	if configPath == "" {
		configPath = filepath.Join(home.Dir(ctx), "config.yaml")
	}
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("Could not load gateway config from %s: %v", configPath, r))
			cfg = map[string]any{}
		}
	}()
	loaded, err := config.LoadUserEffective(configPath, func(name string) (string, bool) { return getenv(ctx, name) })
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not load gateway config from %s: %v", configPath, err))
		return map[string]any{}
	}
	return loaded
}

// Effective gateway config.yaml mapping for the CLI packages, empty on any failure
func LoadGatewayConfigMap(ctx context.Context, configPath string) map[string]any {
	return loadGatewayConfig(ctx, configPath)
}

// Model of a frozen config document, or the profile's model setting when cfg is nil, empty when none is configured
func resolveGatewayModel(ctx context.Context, cfg map[string]any) string {
	if cfg == nil {
		return settings.Model.Get(ctx)
	}
	modelCfg, ok := cfg["model"]
	if !ok {
		modelCfg = map[string]any{}
	}
	if m, isDict := modelCfg.(map[string]any); isDict {
		modelCfg = m["default"]
		if !pyrepr.Truthy(modelCfg) {
			modelCfg = m["model"]
		}
	}
	s, _ := modelCfg.(string)
	return s
}
