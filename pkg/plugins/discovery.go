package plugins

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Environment variable enabling project local plugin discovery
const EnvEnableProjectPlugins = "ICEBATH_ENABLE_PROJECT_PLUGINS"

// Environment variable that skips every plugin and user hook
const EnvSafeMode = "ICEBATH_SAFE_MODE"

// Environment variable naming the Relay plugins toml override the removed relay plugin pointed at

// Keys of the removed relay plugin that core now owns
var legacyRelayPluginKeys = map[string]bool{"nemo_relay": true, "observability/nemo_relay": true}

// Bundled categories that have their own discovery and never load through the general manager
var bundledSkipNames = map[string]bool{"memory": true, "context_engine": true, "model-providers": true, "cron_providers": true, "computer_use": true, "config_backends": true}

// Per harness manifest directories other agent tools ship inside plugin repos
var foreignHarnessDirs = map[string]bool{".claude-plugin": true, ".codex-plugin": true, ".cursor-plugin": true, ".devin-plugin": true, ".kimi-plugin": true}

// Bundled keys of features that left core and the catalog plugin that replaced them
var leftCoreAliases = map[string]string{"observability/langfuse": "langfuse"}

// Catalog plugin a bundled key that left core now names, empty when none
func legacyPluginAlias(name string) string { return leftCoreAliases[name] }

// Bundled keys that now name the catalog plugin
func legacyPluginKeys(plugin string) []string {
	var out []string
	for key, p := range leftCoreAliases {
		if p == plugin {
			out = append(out, key)
		}
	}
	return out
}

// Install hint for a plugin name that left core, empty when it never did
func movedPluginHint(name string) string {
	for key, plugin := range leftCoreAliases {
		if name == key || name == plugin {
			return fmt.Sprintf("%s moved out of core into the '%s' plugin: install it with `icebath plugins install %s`.", plugin, plugin, plugin)
		}
	}
	return ""
}

// Reports whether a directory name is never a plugin
func isSkippedPluginDir(name string) bool {
	return (strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")) || foreignHarnessDirs[name]
}

// Manifests under a plugins root, one level of category nesting, portable packages included
func ScanDirectory(path, source string, skipNames map[string]bool) []*Manifest {
	return scanDirectory(path, source, skipNames, "", 0)
}

func scanDirectory(path, source string, skipNames map[string]bool, prefix string, depth int) []*Manifest {
	var out []*Manifest
	st, err := os.Stat(path)
	if err != nil || !st.IsDir() {
		return out
	}
	entries, err := pluginDirRead(path)
	if err != nil {
		logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", path, pyErrorText(err)))
		return out
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if isSkippedPluginDir(e.Name()) {
			logDebug(fmt.Sprintf("Skipping %s (dunder or foreign-harness manifest dir)", child))
			continue
		}
		manifestFile, portableFile, hasPortable := "", filepath.Join(child, "plugin.json"), false
		isDir, err := pathlibProbe(child, true)
		if err == nil && (!isDir || (depth == 0 && skipNames[e.Name()])) {
			continue
		}
		for _, n := range []string{"plugin.yaml", "plugin.yml"} {
			if err != nil || manifestFile != "" {
				break
			}
			var exists bool
			if exists, err = pathlibProbe(filepath.Join(child, n), false); exists {
				manifestFile = filepath.Join(child, n)
			}
		}
		if err == nil {
			hasPortable, err = pathlibProbe(portableFile, false)
		}
		if err == nil && !hasPortable {
			if lst, lerr := os.Lstat(portableFile); lerr == nil {
				hasPortable = lst.Mode()&os.ModeSymlink != 0
			} else if !pypath.PathlibIgnoredError(lerr) {
				err = lerr
			}
		}
		if err != nil {
			logWarn(fmt.Sprintf("Skipping unreadable plugin directory %s: %s", child, pyErrorText(err)))
			continue
		}
		switch {
		case manifestFile != "":
			if m := ParseManifestFile(manifestFile, child, source, prefix); m != nil {
				out = append(out, m)
			}
		case hasPortable:
			m, err := portablePluginManifest(child, source, prefix)
			if err != nil {
				logWarn(fmt.Sprintf("Failed to parse %s: %s", portableFile, err))
				continue
			}
			out = append(out, m)
		case depth >= 1:
			logDebug(fmt.Sprintf("Skipping %s (no plugin.yaml, depth cap reached)", child))
		default:
			sub := e.Name()
			if prefix != "" {
				sub = prefix + "/" + e.Name()
			}
			out = append(out, scanDirectory(child, source, nil, sub, depth+1)...)
		}
	}
	return out
}

// Directory manifests of a home in discovery order with bundled Go plugins first
func CollectDirectoryManifests(h string) []*Manifest {
	var out []*Manifest
	out = append(out, bundledManifests()...)
	if dir := bundledOverrideDir(); dir != "" {
		out = append(out, ScanDirectory(dir, "bundled", bundledSkipNames)...)
	}
	out = append(out, ScanDirectory(home.PluginsDir(h), "user", nil)...)
	if envEnabled(EnvEnableProjectPlugins) {
		if cwd, err := os.Getwd(); err == nil {
			out = append(out, ScanDirectory(filepath.Join(cwd, ".icebath", "plugins"), "project", nil)...)
		}
	}
	return out
}

// Later sources win on key collision except a flat impostor claiming a bundled key from another directory
func ResolveManifestWinners(manifests []*Manifest) map[string]*Manifest {
	winners := map[string]*Manifest{}
	for _, m := range manifests {
		key := m.ID()
		if shadowed, ok := winners[key]; ok && shadowed.Source == "bundled" && (m.Source == "user" || m.Source == "project") {
			own, bundled := filepath.Base(m.Path), filepath.Base(shadowed.Path)
			if m.Path != "" && shadowed.Path != "" && own != bundled {
				logWarn(fmt.Sprintf("Ignoring %s plugin at %s: its manifest name '%s' is a bundled plugin's key but the directory is named '%s'; rename the directory to '%s' to override the bundled plugin",
					m.Source, m.Path, key, own, bundled))
				continue
			}
			logInfo(fmt.Sprintf("Plugin '%s' at %s (%s) shadows the bundled copy at %s", key, m.Path, m.Source, shadowed.Path))
		}
		winners[key] = m
	}
	return winners
}

// Routing verdict for one winning manifest
type ManifestGate struct {
	Action  string
	Enabled bool
	Error   string
	Log     string
	Level   string
}

// Error recorded for a plugin listed in both the enabled and the disabled list
const InBothLists = "listed in both plugins.enabled and plugins.disabled — disabled wins"

// Every spelling a config list may hold for a manifest
func manifestNames(m *Manifest) map[string]bool {
	names := map[string]bool{m.ID(): true, m.Name: true}
	for _, k := range legacyPluginKeys(m.ID()) {
		names[k] = true
	}
	return names
}

// Reports whether two string sets intersect
func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// Decides how one winning manifest is handled by the enable gate
func GateManifest(h string, m *Manifest, disabled map[string]bool, enabled map[string]bool, enabledPresent bool) ManifestGate {
	key := m.ID()
	names := manifestNames(m)
	placeholder := func(err, level, log string, enabledFlag bool) ManifestGate {
		return ManifestGate{Action: "placeholder", Enabled: enabledFlag, Error: err, Log: log, Level: level}
	}
	if intersects(names, legacyRelayPluginKeys) {
		err := "removed — Relay lifecycle is owned by Icebath core; configure a standard user or system Relay plugins.toml, or use " + tools.RelayPluginsConfigEnv + " for an explicit user-file override"
		return placeholder(err, "warn", fmt.Sprintf("Refusing to load removed Icebath Relay plugin '%s'; %s", key, err), false)
	}
	if intersects(names, disabled) {
		if enabledPresent && intersects(names, enabled) {
			return placeholder(InBothLists, "warn", fmt.Sprintf("Plugin '%s' is listed in both plugins.enabled and plugins.disabled; the disable "+
				"wins and it will not load — remove it from one of the two lists", key), false)
		}
		return placeholder("disabled via config", "debug", fmt.Sprintf("Skipping disabled plugin '%s'", key), false)
	}
	if m.Kind == "exclusive" {
		return placeholder("exclusive plugin — activate via <category>.provider config", "debug", fmt.Sprintf("Skipping '%s' (exclusive, handled by category discovery)", key), false)
	}
	if m.Kind == "model-provider" {
		return placeholder("", "debug", fmt.Sprintf("Skipping '%s' (model-provider, handled by providers/ discovery)", key), true)
	}
	if m.Source == "bundled" {
		if m.Kind == "backend" {
			return ManifestGate{Action: "load_now"}
		}
		if m.Kind == "platform" {
			return ManifestGate{Action: "defer"}
		}
	}
	if !enabledPresent || !intersects(names, enabled) {
		return placeholder(fmt.Sprintf("not enabled in config (run `icebath plugins enable %s` to activate)", key), "debug", fmt.Sprintf("Skipping '%s' (not in plugins.enabled)", key), false)
	}
	if m.Source != "bundled" {
		if removed := InstalledPluginRemoval(h, m.Name, m.Path); removed != nil {
			reason := removed.Reason
			if reason == "" {
				reason = "no reason recorded"
			}
			err := "removed from the Icebath plugin catalog: " + reason
			return placeholder(err, "warn", fmt.Sprintf("Refusing to load plugin '%s' — %s; run `icebath plugins remove`", key, err), false)
		}
	}
	return ManifestGate{Action: "load"}
}

// The plugins.disabled deny list of a home
func disabledPlugins(cfg map[string]any) map[string]bool {
	list, _ := pluginNameList(cfg, "disabled")
	return toSet(list)
}

// The plugins.enabled allow list of a home and whether the key exists
func enabledPlugins(cfg map[string]any) (map[string]bool, bool) {
	list, ok := pluginNameList(cfg, "enabled")
	if !ok {
		return nil, false
	}
	return toSet(list), true
}

// Keys the loader skips because both config lists name them
func PluginsInBothLists(h string) []string {
	cfg := loadConfigOrEmpty(h)
	disabled := disabledPlugins(cfg)
	enabled, present := enabledPlugins(cfg)
	var out []string
	for key, m := range ResolveManifestWinners(CollectDirectoryManifests(h)) {
		if GateManifest(h, m, disabled, enabled, present).Error == InBothLists {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// Emits a gate verdict log line at its level
func logGate(g ManifestGate) {
	if g.Log == "" {
		return
	}
	if g.Level == "warn" {
		logWarn(g.Log)
	} else {
		logDebug(g.Log)
	}
}
