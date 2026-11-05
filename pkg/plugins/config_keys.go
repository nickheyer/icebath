package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
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
	winners := ResolveManifestWinners(CollectDirectoryManifests(h))
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
	if !usable {
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

// MCP servers the enabled portable plugins of a home declare, after discovery of that home
func PortableMCPServers(h string) (map[string]map[string]any, error) {
	ctx := home.WithOverride(context.Background(), h)
	return ensurePluginsDiscovered(ctx, false).GetPortableMCPServers(), nil
}
