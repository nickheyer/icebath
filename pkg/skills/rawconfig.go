package skills

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Parses of config.yaml keyed by path and backend cache version, oldest insert evicted first
var rawConfigCache = struct {
	mu    sync.Mutex
	m     map[string]map[string]any
	order []string
}{m: map[string]map[string]any{}}

// Bound on cached parses so rotating profile scopes keep their unchanged entries
var rawConfigCacheMax = 128

// Seam for the config document read so tests can count and delay parses
var rawConfigSteps = struct {
	read func(ctx context.Context, configPath string) (any, error)
}{read: config.ReadConfigDoc}

// Drops every cached config.yaml parse
func rawConfigCacheClear() {
	rawConfigCache.mu.Lock()
	rawConfigCache.m = map[string]map[string]any{}
	rawConfigCache.order = nil
	rawConfigCache.mu.Unlock()
}

// Path plus backend cache version identity of config.yaml, false when it is absent or unreadable
func configCacheKey(ctx context.Context, configPath string) (string, bool) {
	version, err := config.ConfigCacheVersionOrNone(ctx, configPath)
	if err != nil {
		panic(err)
	}
	if version == nil {
		return "", false
	}
	parts := []string{config.PyRepr(configPath)}
	for _, v := range version {
		parts = append(parts, config.PyRepr(v))
	}
	return "(" + strings.Join(parts, ", ") + ")", true
}

// Raw config.yaml of a home through a version keyed cache, empty when absent, unreadable or not a mapping
func loadRawConfig(ctx context.Context, h string) map[string]any {
	if snap := config.WorkerConfigSnapshot(); snap != nil {
		return snap
	}
	configPath := home.ConfigPath(h)
	exists, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		panic(err)
	}
	if !exists {
		return map[string]any{}
	}
	key, keyed := configCacheKey(ctx, configPath)
	if keyed {
		rawConfigCache.mu.Lock()
		cached, ok := rawConfigCache.m[key]
		rawConfigCache.mu.Unlock()
		if ok {
			return cached
		}
	}
	doc, err := rawConfigSteps.read(ctx, configPath)
	if err != nil {
		slog.Default().Debug("Could not read skill config", "path", configPath, "err", err)
		return map[string]any{}
	}
	parsed, ok := doc.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if keyed {
		rawConfigCache.mu.Lock()
		if _, present := rawConfigCache.m[key]; !present {
			rawConfigCache.order = append(rawConfigCache.order, key)
		}
		rawConfigCache.m[key] = parsed
		for len(rawConfigCache.m) > rawConfigCacheMax {
			oldest := rawConfigCache.order[0]
			rawConfigCache.order = rawConfigCache.order[1:]
			delete(rawConfigCache.m, oldest)
		}
		rawConfigCache.mu.Unlock()
	}
	return parsed
}

// Resolved external skill dirs keyed by config identity
var externalDirsCache = struct {
	mu sync.Mutex
	m  map[string][]string
}{m: map[string][]string{}}

// Drops the external dirs cache and the raw config cache under it
func externalDirsCacheClear() {
	externalDirsCache.mu.Lock()
	externalDirsCache.m = map[string][]string{}
	externalDirsCache.mu.Unlock()
	rawConfigCacheClear()
}

// Config document the scope reads skills settings from, the live file for a process scope
func (s *Scope) rawConfig() map[string]any {
	if !s.live {
		return map[string]any(s.Config)
	}
	return loadRawConfig(context.Background(), s.Home)
}

// Effective merged config of the scope, reloaded through the config cache for a process scope
func (s *Scope) tree() config.Tree {
	if !s.live {
		return s.Config
	}
	cfg, err := config.LoadReadonly(context.Background(), s.Home, s.Secrets.Lookup())
	if err != nil {
		panic(err)
	}
	return cfg
}
