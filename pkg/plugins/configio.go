package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var configWriteMu sync.Mutex

// Effective configuration of a home with defaults merged and secrets expanded
func loadConfig(h string) (map[string]any, error) {
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	return map[string]any(tree), nil
}

// Effective configuration of a home, empty on any read failure
func loadConfigOrEmpty(h string) map[string]any {
	cfg, err := loadConfig(h)
	if err != nil {
		return map[string]any{}
	}
	return cfg
}

// Raw user configuration document of a home
func readUserConfig(h string) (map[string]any, error) {
	return config.ReadUser(h)
}

// Writes the raw user configuration document of a home
func saveUserConfig(h string, doc map[string]any) error {
	return config.SaveUser(h, doc)
}

// Rewrites the user config of a home under one process wide lock
func updateUserConfig(h string, mutate func(doc map[string]any) error) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	doc, err := readUserConfig(h)
	if err != nil {
		return err
	}
	if err := mutate(doc); err != nil {
		return err
	}
	return saveUserConfig(h, doc)
}

// The plugins section of a config document, empty when absent
func pluginsSection(cfg map[string]any) map[string]any {
	if m := asMap(cfg["plugins"]); m != nil {
		return m
	}
	return map[string]any{}
}

// Set of strings in a plugins list key, nil when the key is absent or malformed
func pluginNameList(cfg map[string]any, key string) ([]string, bool) {
	raw, ok := pluginsSection(cfg)[key]
	if !ok {
		return nil, false
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
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

// Bundled plugins directory from the environment, empty when unset
func bundledPluginsDir() string {
	return strings.TrimSpace(os.Getenv("ICEBATH_BUNDLED_PLUGINS"))
}

// Reports whether an environment variable is set to a truthy value
func envEnabled(name string) bool {
	return truthy(os.Getenv(name))
}

// Secrets file lookup of a home
func secretLookup(h string) func(string) (string, bool) {
	s := config.NewSecrets(h)
	return func(name string) (string, bool) {
		if v, ok := s.Get(name); ok && v != "" {
			return v, true
		}
		return "", false
	}
}

// Writes bytes atomically through a temporary sibling file
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.AtomicWrite(path, data, mode)
}
