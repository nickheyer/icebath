package observability

import (
	"context"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ResolveSessionPlatformHook returns the TUI gateway session's platform, set by the TUI gateway owner
var ResolveSessionPlatformHook func() (string, error)

var commandSurfaces = map[string]string{
	"chat": "cli_slash", "tools": "cli_tools", "config": "cli_config", "skills": "cli_config", "plugins": "cli_config",
	"dashboard": "web", "serve": "web", "gui": "web", "desktop": "web",
}

var settingKinds = map[string]string{"memory": "memory", "curator": "curator", "compression": "compression"}

var offValues = Set("false", "off", "no", "0", "none", "")

var (
	surfaceMu      sync.RWMutex
	processSurface string
	// Joined before the exit stamp, as the interpreter joins its non daemon threads
	featureDisabledWG sync.WaitGroup
	defaultOnOnce     sync.Once
	defaultOnPaths    ValueSet
)

// Records the saved config transitions through the config package's save hook
func init() { config.RegisterConfigSavedObserver(RecordConfigSaved) }

// SetProcessSurface sets the running entry point from the icebath subcommand, nil for chat, unknown clearing it
func SetProcessSurface(command any) {
	surface := ""
	switch c := command.(type) {
	case nil:
		surface = "cli_slash"
	case string:
		if c == "tui_gateway" {
			surface = "tui_gateway"
		} else {
			surface = commandSurfaces[c]
		}
	}
	surfaceMu.Lock()
	processSurface = surface
	surfaceMu.Unlock()
}

// Process surface as set, empty for None
func rawSurface() string {
	surfaceMu.RLock()
	defer surfaceMu.RUnlock()
	return processSurface
}

// CurrentSurface is the surface a user change records under, false when the process has none
func CurrentSurface() (string, bool) {
	switch surface := rawSurface(); surface {
	case "":
		return "", false
	case "web":
		return WebSetupSurface(), true
	case "tui_gateway":
		if ResolveSessionPlatformHook == nil {
			return "tui", true
		}
		platform, err := ResolveSessionPlatformHook()
		if err != nil {
			return "tui", true
		}
		return platform, true
	default:
		return surface, true
	}
}

// Lowercased directory names of the plugin manifests one and two levels under the install plugins tree
func bundledPluginNames() ([]string, error) {
	root := filepath.Join(pm.RepoRoot(), "plugins")
	var names []string
	for _, pattern := range []string{"*/plugin.yaml", "*/*/plugin.yaml"} {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, m := range matches {
			names = append(names, pystr.Lower(filepath.Base(filepath.Dir(m))))
		}
	}
	return names, nil
}

// PluginMetricName is a shipped bundled or catalog plugin name, custom otherwise
func PluginMetricName(raw any) string {
	name := catalogNorm(raw)
	name = name[strings.LastIndex(name, "/")+1:]
	if name != "" && (catalogSafe("bundled_plugin_names").Has(name) || catalogSafe("plugin_catalog_names").Has(name)) {
		return name
	}
	return Custom
}

// DefaultOnSettings lists the dotted default config paths whose shipped value is true
func DefaultOnSettings() ValueSet {
	defaultOnOnce.Do(func() {
		var paths []string
		var walk func(node map[string]any, prefix string)
		walk = func(node map[string]any, prefix string) {
			for key, value := range node {
				if strings.HasPrefix(key, "_") {
					continue
				}
				path := prefix + key
				switch v := value.(type) {
				case map[string]any:
					walk(v, path+".")
				case config.Tree:
					walk(v, path+".")
				case bool:
					if v && metricIdentifier(path, FeatureDisabledNameMaxLength) == path {
						paths = append(paths, path)
					}
				}
			}
		}
		walk(config.Defaults(), "")
		defaultOnPaths = Set(paths...)
	})
	return defaultOnPaths
}

// Value at a dotted path, true when any part is absent as every tracked default is true
func dottedGet(cfg any, path string) any {
	node := cfg
	for _, part := range strings.Split(path, ".") {
		m, ok := asStringMap(node)
		if !ok {
			return true
		}
		next, ok := m[part]
		if !ok {
			return true
		}
		node = next
	}
	return node
}

// A config mapping as a plain map
func asStringMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case config.Tree:
		return m, true
	}
	return nil, false
}

// Whether a saved value reads as on, strings off only for the off spellings
func settingOn(value any) bool {
	if s, ok := value.(string); ok {
		return !offValues.Has(pystr.Lower(pystr.Strip(s)))
	}
	return pyrepr.Truthy(value)
}

// Transition is one shipped on thing a write turned off or back on
type Transition struct {
	Kind, Name, Event string
}

// Default on settings whose on state the write flipped, unexpanded templates skipped
func settingTransitions(old, new map[string]any, yield func(Transition)) {
	paths := DefaultOnSettings().Sorted()
	for _, path := range paths {
		before, after := dottedGet(old, path), dottedGet(new, path)
		templated := false
		for _, v := range []any{before, after} {
			if s, ok := v.(string); ok && strings.Contains(s, "${") {
				templated = true
			}
		}
		if templated {
			continue
		}
		b, a := settingOn(before), settingOn(after)
		if b == a {
			continue
		}
		head, _, _ := strings.Cut(path, ".")
		kind, ok := settingKinds[head]
		if !ok {
			kind = "setting"
		}
		event := "disabled"
		if a {
			event = "re_enabled"
		}
		yield(Transition{kind, path, event})
	}
}

// Names a section's list holds, plus every per platform list when one is named
func configNames(cfg map[string]any, section, key, perPlatform string) map[string]bool {
	out := map[string]bool{}
	block, ok := asStringMap(cfg[section])
	if !ok {
		return out
	}
	for _, n := range tools.ParseConfigStringList(block[key]) {
		out[n] = true
	}
	if perPlatform != "" {
		if platforms, ok := asStringMap(block[perPlatform]); ok {
			for _, v := range platforms {
				for _, n := range tools.ParseConfigStringList(v) {
					out[n] = true
				}
			}
		}
	}
	return out
}

// A messaging platform plugin reports as a platform, other plugins as plugins
func pluginKind(name string) string {
	platforms := catalogSafe("bundled_platform_names").Union(catalogSafe("catalog_platform_names"))
	n := pystr.Lower(pystr.Strip(name))
	if platforms.Has(n[strings.LastIndex(n, "/")+1:]) {
		return "platform"
	}
	return "plugin"
}

// Names added to a disabled list read disabled, names removed read re_enabled, each sorted
func listTransitions(oldNames, newNames map[string]bool, kind string, namer func(any) string, yield func(Transition)) {
	for _, step := range []struct {
		from, to map[string]bool
		event    string
	}{{oldNames, newNames, "disabled"}, {newNames, oldNames, "re_enabled"}} {
		var names []string
		for n := range step.to {
			if !step.from[n] {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			k := kind
			if kind == "plugin" {
				k = pluginKind(n)
			}
			yield(Transition{k, namer(n), step.event})
		}
	}
}

// Platform toolsets and disabled toolsets of a config, the scope a toolset change lives in
func toolsetScope(cfg map[string]any) [2]any {
	agent, ok := asStringMap(cfg["agent"])
	if !ok {
		agent = map[string]any{}
	}
	return [2]any{cfg["platform_toolsets"], agent["disabled_toolsets"]}
}

// Plugin toolset keys of the tools configurator, a seam tests replace
var pluginToolsetKeys = tools.PluginToolsetKeys

// Shipped default toolsets a write turned off or back on for each platform the configs name
func toolsetTransitions(ctx context.Context, old, new map[string]any, yield func(Transition)) {
	if reflect.DeepEqual(toolsetScope(old), toolsetScope(new)) {
		return
	}
	platforms := map[string]bool{"cli": true}
	for _, cfg := range []map[string]any{old, new} {
		if m, ok := asStringMap(cfg["platform_toolsets"]); ok {
			for p := range m {
				platforms[p] = true
			}
		}
	}
	shipped := map[string]bool{}
	for _, c := range tools.ConfigurableToolsets {
		shipped[c.Key] = true
	}
	opts := tools.PlatformToolsOptions{PluginToolsetKeys: pluginToolsetKeys(ctx)}
	names := make([]string, 0, len(platforms))
	for p := range platforms {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, platform := range names {
		var defaults []string
		for ts := range tools.Default.PlatformTools(ctx, "", config.Tree{}, platform, opts) {
			if shipped[ts] {
				defaults = append(defaults, ts)
			}
		}
		sort.Strings(defaults)
		before := tools.Default.PlatformTools(ctx, "", config.Tree(old), platform, opts)
		after := tools.Default.PlatformTools(ctx, "", config.Tree(new), platform, opts)
		for _, ts := range defaults {
			if before[ts] != after[ts] {
				event := "disabled"
				if after[ts] {
					event = "re_enabled"
				}
				yield(Transition{"toolset", ts, event})
			}
		}
	}
}

// ConfigTransitions lists each shipped on thing the write turned off or back on, deduplicated
func ConfigTransitions(ctx context.Context, old, new any) []Transition {
	oldMap, ok := asStringMap(old)
	if !ok {
		oldMap = map[string]any{}
	}
	newMap, ok := asStringMap(new)
	if !ok {
		newMap = map[string]any{}
	}
	var found []Transition
	add := func(t Transition) {
		for _, f := range found {
			if f == t {
				return
			}
		}
		found = append(found, t)
	}
	batches := []func(yield func(Transition)){
		func(y func(Transition)) { settingTransitions(oldMap, newMap, y) },
		func(y func(Transition)) {
			listTransitions(configNames(oldMap, "skills", "disabled", "platform_disabled"),
				configNames(newMap, "skills", "disabled", "platform_disabled"), "skill", SkillMetricName, y)
		},
		func(y func(Transition)) {
			listTransitions(configNames(oldMap, "plugins", "disabled", ""), configNames(newMap, "plugins", "disabled", ""),
				"plugin", PluginMetricName, y)
		},
		func(y func(Transition)) { toolsetTransitions(ctx, oldMap, newMap, y) },
	}
	for _, batch := range batches {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Debug("Feature-disabled transitions skipped", "error", r)
				}
			}()
			batch(add)
		}()
	}
	return found
}

// Records one feature disabled mark per transition on a surface
func emitTransitions(ctx context.Context, transitions []Transition, surface string) {
	for _, t := range transitions {
		recordProcessMark(ctx, FeatureDisabledMark, map[string]string{"event": t.Event, "kind": t.Kind, "name": t.Name, "surface": surface})
	}
}

// Diffs and records a saved config under the profile home that owned the write
func recordTransitions(old, new any, surface, homeDir string) {
	ctx := home.WithOverride(context.Background(), homeDir)
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Feature-disabled not recorded", "error", r)
		}
	}()
	emitTransitions(ctx, ConfigTransitions(ctx, old, new), surface)
}

// RecordConfigSaved is the config save hook, diffing off the writer in the home that owns the write
func RecordConfigSaved(h string, oldRaw, newConfig map[string]any) {
	if rawSurface() == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Feature-disabled not recorded", "error", r)
		}
	}()
	ctx := home.WithOverride(context.Background(), h)
	if !relayEnabled(ctx) {
		return
	}
	surface, ok := CurrentSurface()
	if !ok {
		return
	}
	old, new := pycopy.DeepCopy(oldRaw), pycopy.DeepCopy(newConfig)
	featureDisabledWG.Add(1)
	go func() {
		defer featureDisabledWG.Done()
		recordTransitions(old, new, surface, h)
	}()
}

// RecordSkillRemoved counts a catalog skill being uninstalled, hub skills never shipped and never counted
func RecordSkillRemoved(name any) {
	if rawSurface() == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Skill removal not recorded", "error", r)
		}
	}()
	ctx := context.Background()
	if !relayEnabled(ctx) {
		return
	}
	surface, ok := CurrentSurface()
	if !ok {
		return
	}
	if metricName := SkillMetricName(name); metricName != Custom {
		emitTransitions(ctx, []Transition{{"skill", metricName, "disabled"}}, surface)
	}
}
