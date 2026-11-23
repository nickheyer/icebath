package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Names the plugins.disabled list of a home parks
func DisabledPlugins(h string) map[string]bool {
	return disabledPlugins(loadConfigOrEmpty(h))
}

// Registers the settings blocks every discovered plugin and the selected memory provider declare
func RegisterActiveSettings(h string) {
	manifests := CollectDirectoryManifests(h)
	directoryKeys := map[string]bool{}
	for _, m := range manifests {
		directoryKeys[m.ID()] = true
	}
	for _, m := range DiscoverEntrypointManifests(home.WithOverride(context.Background(), h)) {
		if !directoryKeys[m.ID()] {
			manifests = append(manifests, m)
		}
	}
	ResolveManifestWinners(manifests)
	if m := selectedMemoryProviderManifest(h, loadConfigOrEmpty(h)); m != nil {
		settings.RegisterPluginKeys(m.Name, m.ConfigKeys)
	}
}

// Manifest of the memory provider memory.provider selects unless missing or parked in plugins.disabled
func selectedMemoryProviderManifest(h string, cfg map[string]any) *Manifest {
	section, _ := cfg["memory"].(map[string]any)
	name, _ := section["provider"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	s := installedServices()
	if s == nil || s.MemoryProviderDir == nil {
		panic(&ServiceMissingError{Name: "MemoryProviderDir"})
	}
	dir, usable := s.MemoryProviderDir(h, name)
	if !usable || memoryProviderExplicitlyDisabled(cfg, name, dir) {
		return nil
	}
	for _, file := range []string{"plugin.yaml", "plugin.yml"} {
		path := filepath.Join(dir, file)
		if _, err := os.Stat(path); err == nil {
			return ParseManifestFile(path, dir, "memory", "memory")
		}
	}
	return nil
}

// Whether a provider directory sits directly in the bundled memory plugins directory
func bundledMemoryProviderDir(pluginDir string) bool {
	dir, err := pypath.Realpath(pluginDir, pypath.Lax)
	if err != nil {
		dir = pluginDir
	}
	override := bundledOverrideDir()
	if override == "" {
		return false
	}
	root := filepath.Join(override, "memory")
	if resolved, err := pypath.Realpath(root, pypath.Lax); err == nil {
		root = resolved
	}
	return filepath.Dir(dir) == root
}

// True when a non bundled provider directory is parked in plugins.disabled under any spelling the plugin commands write
func memoryProviderExplicitlyDisabled(cfg map[string]any, name, dir string) bool {
	if bundledMemoryProviderDir(dir) {
		return false
	}
	section, _ := cfg["plugins"].(map[string]any)
	disabled, ok := section["disabled"].([]any)
	if !ok {
		return false
	}
	spellings, err := pm.PluginSpellings(dir, name)
	if err != nil {
		panic(err)
	}
	for _, v := range disabled {
		if s, ok := v.(string); ok && spellings[s] {
			return true
		}
	}
	return false
}

// MCP servers the enabled portable plugins of a home declare, after discovery of that home
func PortableMCPServers(h string) (map[string]map[string]any, error) {
	ctx := home.WithOverride(context.Background(), h)
	return ensurePluginsDiscovered(ctx, false).GetPortableMCPServers(), nil
}
