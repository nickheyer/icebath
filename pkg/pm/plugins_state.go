package pm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	ry "github.com/nickheyer/icebath/pkg/internal/ruamelyaml"
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

type runtimeKey struct{}

// Context of code running as the dependency runtime, the far side of the worker boundary and the pm command
func withRuntime(ctx context.Context) context.Context {
	return context.WithValue(ctx, runtimeKey{}, true)
}

// Profiles dir under the home root that plugin discovery and dependency publication share
func profilesRoot(ctx context.Context) string {
	return filepath.Join(DependencyHomeRoot(ctx), "profiles")
}

// Version and document readers of a home selection, the selected backend except inside the dependency runtime of a deployment with no local config layer, which reads the file it publishes to
func selectionReaders(ctx context.Context, configPath string) (version func(context.Context, string) ([]any, error), read func(context.Context, string) (any, error)) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv(config.BackendEnv)))
	if inRuntime, _ := ctx.Value(runtimeKey{}).(bool); kind == "" || kind == "file" || !inRuntime {
		return config.ConfigVersion, config.ReadConfigDoc
	}
	backend, layerHome := config.LocalFileLayer(configPath)
	return func(ctx context.Context, _ string) ([]any, error) { return backend.Version(ctx, layerHome) },
		func(ctx context.Context, _ string) (any, error) { return backend.ReadUserDoc(ctx, layerHome) }
}

// Plugin and memory selection a home's config declares, nil when the home has no config yet
func ReadHomeSelection(ctx context.Context, homeDir string) (map[string]any, error) {
	configPath := filepath.Join(homeDir, "config.yaml")
	configVersion, readConfigDoc := selectionReaders(ctx, configPath)
	unreadable := &PyError{Class: "ValueError", Message: "could not read plugin selection: " + configPath}
	if _, err := configVersion(ctx, configPath); err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil
		case config.IsOSError(err):
			return nil, unreadable
		}
		return nil, err
	}
	doc, err := readConfigDoc(ctx, configPath)
	if err != nil {
		var load *config.YAMLError
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil
		case config.IsOSError(err), errors.As(err, &load) && strings.HasPrefix(load.PyTypeName(), "Unicode"):
			return nil, unreadable
		case errors.As(err, &load) && ry.IsYAMLError(load.Err):
			return nil, &PyError{Class: "ValueError", Message: "could not parse plugin selection: " + configPath}
		}
		return nil, err
	}
	if doc == nil {
		return map[string]any{}, nil
	}
	selection, ok := doc.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "ValueError", Message: "configuration must be a mapping: " + configPath}
	}
	for _, section := range []string{"plugins", "memory"} {
		if value := selection[section]; value != nil {
			if _, ok := value.(map[string]any); !ok {
				return nil, &PyError{Class: "ValueError", Message: section + " must be a mapping: " + configPath}
			}
		}
	}
	plugins, _ := selection["plugins"].(map[string]any)
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
	memory, _ := selection["memory"].(map[string]any)
	if provider := memory["provider"]; provider != nil {
		if _, ok := provider.(string); !ok {
			return nil, &PyError{Class: "ValueError", Message: "memory.provider must be a name: " + configPath}
		}
	}
	return selection, nil
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
func enabledFromConfig(selection map[string]any) []string {
	plugins, ok := selection["plugins"].(map[string]any)
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
		selection, err := ReadHomeSelection(ctx, homeDir)
		if err != nil {
			var pyErr *PyError
			if !opts.SkipInvalidSecondary || i == 0 || !errors.As(err, &pyErr) || pyErr.Class != "ValueError" {
				return nil, err
			}
			pkgLog().Warn("Skipping broken secondary profile during dependency verification", "home", homeDir, "error", err.Error())
			continue
		}
		if selection == nil {
			selection = map[string]any{}
		}
		if opts.ProposedHome != "" && resolvePath(homeDir) == resolvePath(opts.ProposedHome) {
			proposed := make(map[string]any, len(selection)+1)
			for k, v := range selection {
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
			selection = proposed
		}
		names := enabledFromConfig(selection)
		provider, err := providerFromConfig(homeDir, selection, opts.Installing)
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
func providerFromConfig(homeDir string, selection map[string]any, installing string) (string, error) {
	memory, _ := selection["memory"].(map[string]any)
	provider, _ := memory["provider"].(string)
	name := strings.TrimSpace(provider)
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
	plugins, _ := selection["plugins"].(map[string]any)
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

// Spellings of a plugin directory and key that other packages match against plugins.disabled
func PluginSpellings(pluginDir, key string) (map[string]bool, error) {
	return pluginSpellings(pluginDir, key)
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
