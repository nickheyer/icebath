package tuiserver

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
)

// config.yaml of the active profile
func activeConfigPath(ctx context.Context) string { return filepath.Join(home.Dir(ctx), "config.yaml") }

// Raw config cache keyed on the resolved path and the backend cache version, a save write back source
var rawCfgCache struct {
	sync.Mutex
	cfg  map[string]any
	sig  []any
	path string
}

// The active profile's config exactly as written, an error standing for the failed read a save refuses
func loadCfgRaw(ctx context.Context) (map[string]any, error) {
	path := activeConfigPath(ctx)
	sig, err := config.ConfigCacheVersion(ctx, path)
	if errors.Is(err, fs.ErrNotExist) {
		sig, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	rawCfgCache.Lock()
	if rawCfgCache.cfg != nil && reflect.DeepEqual(rawCfgCache.sig, sig) && rawCfgCache.path == path {
		cached := pycopy.DeepCopy(rawCfgCache.cfg).(map[string]any)
		rawCfgCache.Unlock()
		return cached, nil
	}
	rawCfgCache.Unlock()
	exists, err := config.ConfigExists(ctx, path)
	if err != nil {
		return nil, err
	}
	data := map[string]any{}
	if exists {
		if data, err = config.ReadUserConfigRaw(ctx, path); err != nil {
			return nil, err
		}
	}
	rawCfgCache.Lock()
	rawCfgCache.cfg, rawCfgCache.sig, rawCfgCache.path = pycopy.DeepCopy(data).(map[string]any), sig, path
	rawCfgCache.Unlock()
	return data, nil
}

// Replaces the active profile's config with a raw document and caches what the backend kept
func saveCfg(ctx context.Context, cfg map[string]any) error {
	path := activeConfigPath(ctx)
	if err := config.AtomicConfigReplace(ctx, home.Dir(ctx), path, cfg, ""); err != nil {
		return err
	}
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if !fileTooling {
		dropCfgCache()
		return nil
	}
	sig, sigErr := config.ConfigCacheVersion(ctx, path)
	if sigErr != nil {
		sig = nil
	}
	rawCfgCache.Lock()
	defer rawCfgCache.Unlock()
	rawCfgCache.cfg, rawCfgCache.path, rawCfgCache.sig = pycopy.DeepCopy(cfg).(map[string]any), path, sig
	return nil
}

// Forgets the cached raw config so the next raw read returns what the backend holds
func dropCfgCache() {
	rawCfgCache.Lock()
	defer rawCfgCache.Unlock()
	rawCfgCache.cfg, rawCfgCache.sig, rawCfgCache.path = nil, nil, ""
}

// One keyed backend write setting and unsetting exactly these dotted keys, nothing applied when it fails
func writeConfigChanges(ctx context.Context, sets []config.KeyValue, unsets []string) error {
	defer dropCfgCache()
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
