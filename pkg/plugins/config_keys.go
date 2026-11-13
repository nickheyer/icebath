package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Names the plugins.disabled list of a home parks
func DisabledPlugins(h string) map[string]bool {
	return disabledPlugins(loadConfigOrEmpty(h))
}

// Manifests the loader would activate plus the selected memory provider, as config key owners
func ActiveConfigKeyManifests(h string) []config.PluginConfigKeys {
	cfg := loadConfigOrEmpty(h)
	disabled := disabledPlugins(cfg)
	enabled, enabledPresent := enabledPlugins(cfg)
	var out []config.PluginConfigKeys
	manifests := CollectDirectoryManifests(h)
	directoryKeys := map[string]bool{}
	for _, m := range manifests {
		directoryKeys[m.ID()] = true
	}
	winners := ResolveManifestWinners(manifests)
	for _, key := range sortedKeys(winners) {
		m := winners[key]
		gate := GateManifest(h, m, disabled, enabled, enabledPresent)
		if (gate.Action != "placeholder" || gate.Enabled) && RequiresVersionError(m.RequiresIcebath) == "" {
			out = append(out, config.PluginConfigKeys{Name: m.Name, ConfigKeys: m.ConfigKeys})
		}
	}
	if m := selectedMemoryProviderManifest(h, cfg); m != nil {
		out = append(out, config.PluginConfigKeys{Name: m.Name, ConfigKeys: m.ConfigKeys})
	}
	return out
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
