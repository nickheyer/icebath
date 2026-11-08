package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
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
			slog.Debug(fmt.Sprintf("Could not load gateway config from %s: %v", configPath, r))
			cfg = map[string]any{}
		}
	}()
	loaded, err := config.LoadUserEffective(configPath, func(name string) (string, bool) { return getenv(ctx, name) })
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not load gateway config from %s: %v", configPath, err))
		return map[string]any{}
	}
	return loaded
}

// Profile model of a config or the gateway config, else the claimed welcome trial model
func resolveGatewayModel(ctx context.Context, cfg map[string]any) string {
	if cfg == nil {
		cfg = loadGatewayConfig(ctx, "")
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
	if s, isStr := modelCfg.(string); isStr && s != "" {
		return s
	}
	return provider.TrialModel(ctx, home.Dir(ctx))
}
