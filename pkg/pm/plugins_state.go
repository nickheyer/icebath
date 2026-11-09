package pm

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Plugins config key mapping each plugin an update disabled to what to undo once it fits
const DisabledByUpdateRecord = "disabled_by_update"

// HomePlugins is one home's plugins dir with its enabled plugin names in config order
type HomePlugins struct {
	PluginsDir string
	Names      []string
}

// EnabledPluginsOptions propose a selection for one home, name a plugin being installed and tolerate broken secondaries
type EnabledPluginsOptions struct {
	ProposedHome         string
	Enabled              []string
	Disabled             []string
	Installing           string
	SkipInvalidSecondary bool
}

// YAML parser a home selection is read with, one parse per home
var selectionYAMLUnmarshal = yaml.Unmarshal

// Profiles dir under the home root that plugin discovery and dependency publication share
func profilesRoot(ctx context.Context) string {
	return filepath.Join(DependencyHomeRoot(ctx), "profiles")
}

// Plugin and memory selection a home's config.yaml declares, nil when the home has no config yet
func ReadHomeSelection(homeDir string) (map[string]any, error) {
	configPath := filepath.Join(homeDir, "config.yaml")
	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, &PyError{Class: "ValueError", Message: "could not read plugin selection: " + configPath}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, &PyError{Class: "ValueError", Message: "could not read plugin selection: " + configPath}
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if strictUTF8(data) != nil {
		return nil, &PyError{Class: "ValueError", Message: "could not read plugin selection: " + configPath}
	}
	var doc any
	if err := selectionYAMLUnmarshal(data, &doc); err != nil {
		return nil, &PyError{Class: "ValueError", Message: "could not parse plugin selection: " + configPath}
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	config, ok := yamlNormalize(doc).(map[string]any)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: "configuration must be a mapping: " + configPath}
	}
	for _, section := range []string{"plugins", "memory"} {
		if value := config[section]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				return nil, &PyError{Class: "ValueError", Message: section + " must be a mapping: " + configPath}
			}
		}
	}
	plugins, _ := config["plugins"].(map[string]any)
	for _, key := range []string{"enabled", "disabled"} {
		names := plugins[key]
		if names == nil {
			continue
		}
		list, ok := names.([]any)
		for _, name := range list {
			if _, isString := name.(string); !isString {
				ok = false
			}
		}
		if !ok {
			return nil, &PyError{Class: "ValueError", Message: "plugins." + key + " must be a list of names: " + configPath}
		}
	}
	memory, _ := config["memory"].(map[string]any)
	if provider := memory["provider"]; provider != nil {
		if _, ok := provider.(string); !ok {
			return nil, &PyError{Class: "ValueError", Message: "memory.provider must be a name: " + configPath}
		}
	}
	return config, nil
}

// String items of a decoded list as a set, other items never equal to a plugin name
func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	items, _ := v.([]any)
	for _, item := range items {
		if s, ok := item.(string); ok {
			out[s] = true
		}
	}
	return out
}

// Enabled plugin names of a parsed config in order, duplicates and disabled names or leaves dropped
func enabledFromConfig(config map[string]any) []string {
	plugins, ok := config["plugins"].(map[string]any)
	if !ok {
		return nil
	}
	enabled, ok := plugins["enabled"].([]any)
	if !ok {
		return nil
	}
	disabled := stringSet(plugins["disabled"])
	var out []string
	seen := map[string]bool{}
	for _, item := range enabled {
		name, ok := item.(string)
		if !ok || name == "" || seen[name] || disabled[name] {
			continue
		}
		if disabled[name[strings.LastIndex(name, "/")+1:]] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Whether a path is a directory, only a missing path reading as absent and other stat failures refused
func pluginIsDirectory(path string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, &PyError{Class: "ValueError", Message: "could not inspect plugin directory: " + path}
	}
	return st.IsDir(), nil
}

// Default home plus every live profile whose selection feeds the shared environment, all or an error
func dependencyHomes(ctx context.Context) ([]string, error) {
	homes := []string{DependencyHomeRoot(ctx)}
	root := profilesRoot(ctx)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return homes, nil
		}
		return nil, &PyError{Class: "ValueError", Message: "could not enumerate profiles: " + root}
	}
	profiles := make([]string, len(entries))
	for i, entry := range entries {
		profiles[i] = filepath.Join(root, entry.Name())
	}
	sort.Strings(profiles)
	for _, profile := range profiles {
		isDir, err := pluginIsDirectory(profile)
		if err != nil {
			return nil, err
		}
		name := filepath.Base(profile)
		if !isDir || name == "default" || !home.ProfileIDRe.MatchString(strings.TrimSuffix(name, "\n")) || !home.ProfileLive(profile) {
			continue
		}
		homes = append(homes, profile)
	}
	return homes, nil
}

// DependencyHomes exports the dependency home union for the plugin manager hooks of provider resolution
func DependencyHomes(ctx context.Context) ([]string, error) { return dependencyHomes(ctx) }

// Every home's plugins dir with its ordered enabled names, the active memory provider joining its home's list
func EnabledPluginsOrdered(ctx context.Context, opts EnabledPluginsOptions) ([]HomePlugins, error) {
	homes, err := dependencyHomes(ctx)
	if err != nil {
		return nil, err
	}
	var out []HomePlugins
	for i, homeDir := range homes {
		config, err := ReadHomeSelection(homeDir)
		if err != nil {
			var pyErr *PyError
			if !opts.SkipInvalidSecondary || i == 0 || !errors.As(err, &pyErr) || pyErr.Class != "ValueError" {
				return nil, err
			}
			slog.Warn("Skipping broken secondary profile during dependency verification", "home", homeDir, "error", err.Error())
			continue
		}
		if config == nil {
			config = map[string]any{}
		}
		if opts.ProposedHome != "" && resolvePath(homeDir) == resolvePath(opts.ProposedHome) {
			proposed := make(map[string]any, len(config)+1)
			for k, v := range config {
				proposed[k] = v
			}
			enabled := make([]any, len(opts.Enabled))
			for j, name := range opts.Enabled {
				enabled[j] = name
			}
			disabled := make([]any, len(opts.Disabled))
			for j, name := range opts.Disabled {
				disabled[j] = name
			}
			proposed["plugins"] = map[string]any{"enabled": enabled, "disabled": disabled}
			config = proposed
		}
		names := enabledFromConfig(config)
		provider, err := providerFromConfig(homeDir, config, opts.Installing)
		if err != nil {
			return nil, err
		}
		if provider != "" && !containsString(names, provider) {
			names = append(names, provider)
		}
		if len(names) > 0 {
			out = append(out, HomePlugins{PluginsDir: filepath.Join(homeDir, "plugins"), Names: names})
		}
	}
	return out, nil
}

// Memory provider of a parsed config when its plugin dir exists and plugins.disabled does not park it
func providerFromConfig(homeDir string, config map[string]any, installing string) (string, error) {
	memory, _ := config["memory"].(map[string]any)
	provider, _ := memory["provider"].(string)
	name := pystr.Strip(provider)
	if name == "" {
		return "", nil
	}
	pluginDir := filepath.Join(homeDir, "plugins", name)
	if installing != "" && resolvePath(pluginDir) == resolvePath(installing) {
		return name, nil
	}
	isDir, err := pluginIsDirectory(pluginDir)
	if err != nil || !isDir {
		return "", err
	}
	plugins, _ := config["plugins"].(map[string]any)
	disabled := stringSet(plugins["disabled"])
	spellings, err := pluginSpellings(pluginDir, "")
	if err != nil {
		return "", err
	}
	for spelling := range spellings {
		if disabled[spelling] {
			return "", nil
		}
	}
	return name, nil
}

// Every name a plugins list may use for a plugin, its key and leaf, its dir name and its readable manifest name
func pluginSpellings(pluginDir, key string) (map[string]bool, error) {
	names := map[string]bool{filepath.Base(pluginDir): true}
	if key != "" {
		names[key] = true
		names[key[strings.LastIndex(key, "/")+1:]] = true
	}
	manifestFile, err := NativeManifestFile(pluginDir)
	var manifest map[string]any
	if err == nil && manifestFile != "" {
		manifest, err = ReadNativeManifest(manifestFile)
	}
	var pyErr *PyError
	switch {
	case err != nil && !(errors.As(err, &pyErr) && pyErr.Class == "ValueError"):
		return nil, err
	case err == nil && manifest != nil:
		name := ""
		if pyrepr.Truthy(manifest["name"]) {
			name = pyrepr.Str(manifest["name"])
		}
		names[name] = true
	}
	delete(names, "")
	return names, nil
}
