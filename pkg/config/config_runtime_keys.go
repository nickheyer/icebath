package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// Core owned keys read at runtime but absent from the defaults, mapped to their reader
var CoreRuntimeKeys = map[string]string{
	"computer_use.backend": "plugins/computer_use/__init__.py configured_backend_name() (unset = cua)",
	"dashboard.kanban":     "plugins/kanban/dashboard/plugin_api.py (bundled dashboard tab preferences)",
}

// Config keys one active plugin manifest declares
type PluginConfigKeys struct {
	Name       string
	ConfigKeys []string
}

// Owners of runtime read keys outside the schema, supplied by the moa, tools and plugin packages
type RuntimeKeyOwners struct {
	IsPresetFieldPath   func(parts []string) bool
	SelectionConfigKeys func() map[string]bool
	ActiveManifests     func() []PluginConfigKeys
}

// Reports whether key equals a path or sits below one
func coveredBy[V any](key string, paths map[string]V) bool {
	for path := range paths {
		if key == path || strings.HasPrefix(key, path+".") {
			return true
		}
	}
	return false
}

// Reports whether a dotted path exists in the defaults through mappings
func InDefaultConfig(path string) bool {
	_, ok := CfgGet(Defaults(), strings.Split(path, ".")...)
	return ok
}

// Config keys active plugins declare minus paths the defaults already define
func PluginDeclaredConfigKeys(manifests []PluginConfigKeys) map[string]bool {
	keys := map[string]bool{}
	for _, m := range manifests {
		for _, path := range m.ConfigKeys {
			if InDefaultConfig(path) {
				slog.Debug(fmt.Sprintf("config_keys: ignoring %s from plugin %s (a DEFAULT_CONFIG key)", path, m.Name))
				continue
			}
			keys[path] = true
		}
	}
	return keys
}

// Reports whether a schema refused key is a moa preset field, a selection or core runtime key, or plugin declared
func IsRuntimeReadKey(parts []string, owners RuntimeKeyOwners) bool {
	if owners.IsPresetFieldPath(parts) {
		return true
	}
	dotted := strings.Join(parts, ".")
	if owners.SelectionConfigKeys()[dotted] || coveredBy(dotted, CoreRuntimeKeys) {
		return true
	}
	return coveredBy(dotted, PluginDeclaredConfigKeys(owners.ActiveManifests()))
}
