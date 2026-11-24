package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Names a plugin list setting holds for the profile of ctx, as a set
func nameSet(ctx context.Context, list *settings.List) map[string]bool {
	return toSet(list.Strings(ctx))
}

// The plugins.entries block of one plugin for the profile of ctx, empty when absent or not a mapping
func pluginEntry(ctx context.Context, pluginID string) map[string]any {
	if e := asMap(settings.PluginsEntries.Get(ctx)[pluginID]); e != nil {
		return e
	}
	return map[string]any{}
}

// A plugin's own free-form block at a config key for the profile of ctx, the map setting its manifest registered; empty when none is registered there
func pluginBlock(ctx context.Context, key string) map[string]any {
	if block, ok := settings.FindKey(key).(*settings.Map); ok {
		return block.Get(ctx)
	}
	return map[string]any{}
}

// Writes one field of a plugin's plugins.entries block into the profile of ctx through the settings writer, a dotted field nesting
func setPluginEntryField(ctx context.Context, pluginID, field string, value any) error {
	_, err := settings.Set(ctx, settings.PluginsEntries.EntryKey(pluginID)+"."+field, value, settings.Global)
	return err
}

// Set form of a string slice
func toSet(items []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range items {
		out[s] = true
	}
	return out
}

// Sorted keys of a boolean set
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}

// Sorts a string slice in place
func sortStrings(items []string) { sort.Strings(items) }

// Any list of strings as a YAML list value
func anyList(items []string) []any {
	out := make([]any, 0, len(items))
	for _, s := range items {
		out = append(out, s)
	}
	return out
}

// Active profile name of a home, default when it is the root
func profileName(h string) string {
	if n := home.ProfileNameFor(h); n != "" {
		return n
	}
	return "custom"
}

// The user plugins directory of a home, created on demand
func pluginsDir(h string) (string, error) {
	dir := home.PluginsDir(h)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("could not create %s: %w", dir, err)
	}
	return dir, nil
}

// Bundled plugins directory, ICEBATH_BUNDLED_PLUGINS first, else the plugins tree beside the running install
func GetBundledPluginsDir() string {
	if override := bundledOverrideDir(); override != "" {
		return override
	}
	return filepath.Join(pm.RepoRoot(), "plugins")
}

// Reports whether an environment variable is set to a truthy value
func envEnabled(name string) bool {
	return truthy(os.Getenv(name))
}

// Writes bytes atomically through a temporary sibling file
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.AtomicWrite(path, data, mode)
}
