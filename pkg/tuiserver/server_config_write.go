package tuiserver

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// config.yaml of the active profile
func activeConfigPath(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "config.yaml") }

// The active profile's config exactly as written, the only document a write back may save
func loadCfgRaw(ctx context.Context) (map[string]any, error) {
	path := activeConfigPath(ctx)
	exists, err := config.ConfigExists(ctx, path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[string]any{}, nil
	}
	return config.ReadUserConfigRaw(ctx, path)
}

// Replaces the active profile's config with a raw document
func saveCfg(ctx context.Context, cfg map[string]any) error {
	return config.AtomicConfigReplace(ctx, home.Dir(ctx), activeConfigPath(ctx), cfg, "")
}

// One keyed backend write setting and unsetting exactly these dotted keys, nothing applied when it fails
func writeConfigChanges(ctx context.Context, sets []config.KeyValue, unsets []string) error {
	return config.WriteChanges(ctx, activeConfigPath(ctx), config.Changes{Set: sets, Unset: unsets})
}

// Sets one dotted config key
func writeConfigKey(ctx context.Context, keyPath string, value any) error {
	return writeConfigKeys(ctx, []config.KeyValue{{Key: keyPath, Value: value}})
}

// Sets related dotted keys together, one backend write without a config file else one raw round trip
func writeConfigKeys(ctx context.Context, updates []config.KeyValue) error {
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if !fileTooling {
		return writeConfigChanges(ctx, updates, nil)
	}
	cfg, err := loadCfgRaw(ctx)
	if err != nil {
		return err
	}
	for _, update := range updates {
		parts := strings.Split(update.Key, ".")
		current := cfg
		for _, key := range parts[:len(parts)-1] {
			next, isDict := current[key].(map[string]any)
			if !isDict {
				next = map[string]any{}
				current[key] = next
			}
			current = next
		}
		current[parts[len(parts)-1]] = update.Value
	}
	return saveCfg(ctx, cfg)
}
