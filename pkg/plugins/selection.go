package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Bundled dashboard auth plugin keys that must never sit in the deny list while basic auth is configured
var basicAuthPluginKeys = map[string]bool{"basic": true, "dashboard_auth/basic": true}

// Manifest kinds active by default when bundled
var bundledDefaultOnKinds = map[string]bool{"backend": true, "platform": true, "model-provider": true}

// One selection lock per home so concurrent enables never submit the same config version
var selectionLocks sync.Map

// String config value, empty missing or falsy values coerced to the default
func configStr(h, def string, keys ...string) string {
	v := configValue(h, def, keys...)
	if !pyrepr.Truthy(v) {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Persists config section key to config.yaml, creating the section
func writeConfigValue(h, section, key string, value any) error {
	return updateUserConfig(h, func(doc map[string]any) error {
		existing, present := doc[section]
		if !present {
			existing = map[string]any{}
			doc[section] = existing
		}
		m, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("%s object does not support item assignment", pyrepr.Repr(pyrepr.TypeName(existing)))
		}
		m[key] = value
		return nil
	})
}

// Live memory provider name, empty for the built in one
func getCurrentMemoryProvider(h string) string { return configStr(h, "", "memory", "provider") }

// Selects a memory provider in config.yaml
func saveMemoryProvider(h, name string) error { return writeConfigValue(h, "memory", "provider", name) }

// Configured context engine, compressor when unset
func getCurrentContextEngine(h string) string { return configStr(h, "compressor", "context", "engine") }

// Selects a context engine in config.yaml
func saveContextEngine(h, name string) error { return writeConfigValue(h, "context", "engine", name) }

// Installed plugin path for a name, else exit 1 with the invalid name or the unknown plugin message
func requireInstalledPlugin(ctx context.Context, name, pluginsDir string, console *Console) (string, error) {
	target, err := sanitizePluginName(name, pluginsDir, true)
	if err != nil {
		return "", fail(console, "[red]Error:[/red] "+err.Error())
	}
	if _, err := os.Stat(target); err != nil {
		resolved, err := resolvedUserPluginDir(ctx, name)
		if err != nil {
			return "", err
		}
		if resolved == "" {
			return "", fail(console, unknownPluginMessage(name, true))
		}
		target = resolved
	}
	return target, nil
}

// No plugin named message with the exact name rule and the two commands that resolve it
func unknownPluginMessage(name string, downloadedOnly bool) string {
	scope := " Bundled plugins can only be enabled or disabled."
	if downloadedOnly {
		scope = " This command only works on downloaded plugins; bundled ones can only be enabled or disabled."
	}
	if moved := movedPluginHint(name); moved != "" {
		return fmt.Sprintf("[red]No plugin named '%s'.[/red] %s", name, moved)
	}
	return fmt.Sprintf("[red]No plugin named '%s'.[/red] Run `icebath plugins list` to see the exact names "+
		"(nested plugins use their full key, e.g. web/firecrawl).%s "+
		"To add one: `icebath plugins install <owner/repo>`.", name, scope)
}

// Frozen old updater entry that stops for relaunch instead of writing a raw plugin selection
func saveEnabledSet(ctx context.Context) error { return pm.StopForRelaunch(ctx) }

// Drops the bundled basic dashboard auth plugin from plugins.disabled, true when the config changed
func EnsureBasicAuthPluginEnabledInConfig(cfg map[string]any) bool {
	pluginsCfg, _ := cfg["plugins"].(map[string]any)
	disabled, ok := pluginsCfg["disabled"].([]any)
	if pluginsCfg == nil || !ok {
		return false
	}
	kept, hit := map[string]bool{}, false
	for _, v := range disabled {
		name := scalarString(v)
		if basicAuthPluginKeys[name] {
			hit = true
			continue
		}
		kept[name] = true
	}
	if !hit {
		return false
	}
	pluginsCfg["disabled"] = anyList(sortedSet(kept))
	return true
}

// Removes a key and its bare leaf from a name set
func discardKeyAndLeaf(names map[string]bool, key string) {
	delete(names, key)
	parts := strings.Split(key, "/")
	delete(names, parts[len(parts)-1])
}

// Every spelling a config list may hold for a key, discovering the plugins once
func pluginAliases(h, key string) (map[string]bool, error) {
	entries, err := DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	return aliasesFrom(key, entries), nil
}

// Key, bare leaf, legacy keys and the manifest names of the entries carrying the key
func aliasesFrom(key string, entries []PluginEntry) map[string]bool {
	parts := strings.Split(key, "/")
	names := map[string]bool{key: true, parts[len(parts)-1]: true}
	for _, legacy := range legacyPluginKeys(key) {
		names[legacy] = true
	}
	for _, e := range entries {
		if e.Key == key {
			names[e.Name] = true
		}
	}
	return names
}

// Persists the canonical key through the admission transaction, false when the lists already agree
func activateKey(ctx context.Context, h, key string, enable bool, console *Console) (bool, error) {
	enabled, disabled := getEnabledSet(h), getDisabledSet(h)
	aliases, err := pluginAliases(h, key)
	if err != nil {
		return false, err
	}
	target, other := enabled, disabled
	if !enable {
		target, other = disabled, enabled
	}
	if target[key] && !intersects(aliases, other) {
		byUpdate := false
		if !enable {
			if byUpdate, err = disabledByUpdate(h, key, aliases); err != nil {
				return false, err
			}
		}
		if !byUpdate {
			return false, nil
		}
	}
	return true, setPluginEnabled(ctx, h, key, enable, sortedSet(aliases), console)
}

// Whether an update disabled the plugin and records it for retry
func disabledByUpdate(h, key string, aliases map[string]bool) (bool, error) {
	selection, err := pm.ReadHomeSelection(h)
	if err != nil {
		return false, err
	}
	plugins, _ := selection["plugins"].(map[string]any)
	record, ok := plugins[pm.DisabledByUpdateRecord].(map[string]any)
	if !ok {
		return false, nil
	}
	if _, hit := record[key]; hit {
		return true, nil
	}
	for alias := range aliases {
		if _, hit := record[alias]; hit {
			return true, nil
		}
	}
	return false, nil
}

// Drops every trace of a removed plugin from config.yaml, reporting a cleared memory provider selection
func forgetPluginConfig(h string, aliases map[string]bool) (map[string]any, error) {
	result := map[string]any{}
	err := updateUserConfig(h, func(cfg map[string]any) error {
		if pluginsCfg, ok := cfg["plugins"].(map[string]any); ok {
			for _, listKey := range []string{"enabled", "disabled"} {
				names, ok := pluginsCfg[listKey].([]any)
				if !ok {
					continue
				}
				kept, hit := map[string]bool{}, false
				for _, v := range names {
					name := scalarString(v)
					if aliases[name] {
						hit = true
					} else {
						kept[name] = true
					}
				}
				if hit {
					pluginsCfg[listKey] = anyList(sortedSet(kept))
				}
			}
			if entries, ok := pluginsCfg["entries"].(map[string]any); ok {
				for alias := range aliases {
					delete(entries, alias)
				}
			}
		}
		if memoryCfg, ok := cfg["memory"].(map[string]any); ok {
			provider := ""
			if v := memoryCfg["provider"]; pyrepr.Truthy(v) {
				provider = pystr.Strip(pyrepr.Str(v))
			}
			if aliases[provider] {
				memoryCfg["provider"] = ""
				result = map[string]any{"cleared_memory_provider": true}
			}
		}
		return nil
	})
	return result, err
}

// Per home selection lock keyed by the resolved home
func selectionLock(h string) *sync.Mutex {
	lock, _ := selectionLocks.LoadOrStore(home.Key(h), &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// Submits an enable or disable delta with the selection version it read
func setPluginEnabled(ctx context.Context, h, name string, enable bool, aliases []string, console *Console) error {
	lock := selectionLock(h)
	lock.Lock()
	defer lock.Unlock()
	expected, err := pluginSelectionVersion(h)
	if err != nil {
		return err
	}
	selection, err := pm.ReadHomeSelection(h)
	if err != nil {
		return err
	}
	plugins, _ := selection["plugins"].(map[string]any)
	enabled, disabled := map[string]bool{}, map[string]bool{}
	for _, pair := range []struct {
		key string
		set map[string]bool
	}{{"enabled", enabled}, {"disabled", disabled}} {
		list, _ := plugins[pair.key].([]any)
		for _, v := range list {
			pair.set[scalarString(v)] = true
		}
	}
	applyActivation(enabled, disabled, name, aliases, enable)
	verb, plugin := "Disable", ""
	if enable {
		verb, plugin = "Enable", name
	}
	return admitAndSavePluginSets(ctx, h, enabled, disabled, admitSetOptions{Console: console,
		Action: fmt.Sprintf("%s '%s'", verb, name), ExpectedConfig: expected, Plugin: plugin,
		Settled: append([]string{name}, aliases...)})
}

// Adds the key to the target list and purges it, its bare leaf and the aliases from the other
func applyActivation(enabled, disabled map[string]bool, key string, aliases []string, enable bool) {
	removed, target := disabled, enabled
	if !enable {
		removed, target = enabled, disabled
	}
	discardKeyAndLeaf(removed, key)
	for _, a := range aliases {
		delete(removed, a)
	}
	target[key] = true
}

// Canonical registry key for a manifest name, directory name or path key, empty when unknown
func resolveNamedPluginKey(ctx context.Context, name string) (string, error) {
	key, _, err := resolvePluginKeyAndSource(ctx, name)
	return key, err
}

// Discovered entry a plugin verb resolves a name to under the context home
func findPluginEntry(ctx context.Context, name string) (*discoveredPlugin, error) {
	return FindPluginEntry(home.Dir(ctx), name)
}

// Every plugin the loader sees under the context home
func discoverAllPlugins(ctx context.Context) ([]discoveredPlugin, error) {
	return DiscoverAllPlugins(home.Dir(ctx))
}

// Canonical key and source label of a name, both empty when unknown
func resolvePluginKeyAndSource(ctx context.Context, name string) (string, string, error) {
	entry, err := findPluginEntry(ctx, name)
	if err != nil || entry == nil {
		return "", "", err
	}
	return entry.Key, entry.Source, nil
}

// Enables a plugin, asking capability consent for non bundled ones and syncing saved toolset lists
func CmdEnable(ctx context.Context, name string, allowToolOverride *bool, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	refuseLegacyRelay := func(plugin string) error {
		if legacyRelayPluginKeys[plugin] {
			return fail(console, fmt.Sprintf("[red]Plugin '%s' was removed.[/red] Relay lifecycle is owned "+
				"by Icebath core; configure a standard user or system Relay plugins.toml, or use "+
				"%s for an explicit user-file override.", plugin, tools.RelayPluginsConfigEnv))
		}
		return nil
	}
	if err := refuseLegacyRelay(name); err != nil {
		return err
	}
	key, source, err := resolvePluginKeyAndSource(ctx, name)
	if err != nil {
		return err
	}
	if key == "" {
		return fail(console, unknownPluginMessage(name, false))
	}
	if err := refuseLegacyRelay(key); err != nil {
		return err
	}
	if source != "bundled" {
		parts := strings.Split(key, "/")
		dir, err := userInstalledPluginDir(ctx, parts[len(parts)-1])
		if err != nil {
			return err
		}
		if err := refuseIfInstalledRemoved(h, key, dir); err != nil {
			var opErr *PluginOperationError
			if errors.As(err, &opErr) {
				return fail(console, "[red]Error:[/red] "+opErr.Error())
			}
			return err
		}
	}
	changed, err := activateKey(ctx, h, key, true, console)
	if err != nil {
		return err
	}
	if changed {
		console.Print(fmt.Sprintf("[green]✓[/green] Plugin [bold]%s[/bold] enabled.", key))
		if err := reportActivation(ctx, console, key); err != nil {
			return err
		}
	} else {
		console.Print(fmt.Sprintf("[dim]Plugin '%s' is already enabled.[/dim]", key))
	}
	if source != "bundled" {
		if err := enableConsent(ctx, console, key, allowToolOverride); err != nil {
			return err
		}
	}
	if changed {
		return togglePluginToolset(ctx, h, key, true, "")
	}
	return nil
}

// Capability consent and the legacy tool override grant of a non bundled enable
func enableConsent(ctx context.Context, console *Console, key string, allowToolOverride *bool) error {
	declared, err := declaredCapabilitiesForKey(ctx, key)
	if err != nil {
		return err
	}
	if len(declared) > 0 {
		if err := runCapabilityConsent(ctx, console, key, declared, "enable"); err != nil {
			return err
		}
	}
	if allowToolOverride != nil {
		return resolveToolOverrideGrant(ctx, console, key, allowToolOverride)
	}
	return nil
}

// Disables a plugin, removing its toolset from saved lists and hinting how the gateway applies it
func CmdDisable(ctx context.Context, name string, out io.Writer) error {
	console := NewConsole(out)
	h := home.Dir(ctx)
	key, err := resolveNamedPluginKey(ctx, name)
	if err != nil {
		return err
	}
	if key == "" {
		return fail(console, unknownPluginMessage(name, false))
	}
	toolsetKey, err := savedListToolsetKey(ctx, h, key)
	if err != nil {
		return err
	}
	changed, err := activateKey(ctx, h, key, false, console)
	if err != nil {
		return err
	}
	if !changed {
		console.Print(fmt.Sprintf("[dim]Plugin '%s' is already disabled.[/dim]", key))
		return nil
	}
	if err := togglePluginToolset(ctx, h, key, false, toolsetKey); err != nil {
		return err
	}
	console.Print(fmt.Sprintf("[yellow]⊘[/yellow] Plugin [bold]%s[/bold] disabled.", key))
	console.Print("[dim]" + gatewayReloadHint(reloadRunningGateway(ctx), key) + "[/dim]")
	return nil
}

// Whether a dir is an Agent Plugins v1 package with no native manifest
func isPortablePluginDir(dir string) bool {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return false
	}
	native, err := nativeManifestFile(dir)
	return err == nil && native == "" && hasPortableManifest(dir)
}

// Whether a plugin is active with no plugins.enabled entry
func defaultOn(h, dir, source, key string) (bool, error) {
	if dir == "" && source == "bundled" {
		for _, m := range bundledManifests() {
			if m.Key == key {
				kind := strings.ToLower(strings.TrimSpace(m.Kind))
				return kind == "model-provider" || bundledDefaultOnKinds[kind], nil
			}
		}
		return false, nil
	}
	if st, err := os.Stat(dir); dir == "" || err != nil || !st.IsDir() {
		return false, nil
	}
	if source != "bundled" {
		root, err := pluginsDir(h)
		if err != nil {
			return false, err
		}
		base, parent := filepath.Base(dir), filepath.Dir(filepath.Clean(dir))
		if strings.HasPrefix(base, "_") || strings.HasPrefix(base, ".") ||
			(parent != filepath.Clean(root) && parent != filepath.Join(root, "model-providers")) {
			return false, nil
		}
		if parent == filepath.Clean(root) {
			svc := installedServices()
			if svc == nil || svc.DeclaresModelProviderKind == nil {
				return false, &ServiceMissingError{Name: "DeclaresModelProviderKind"}
			}
			return svc.DeclaresModelProviderKind(dir), nil
		}
	}
	manifestFile, err := nativeManifestFile(dir)
	if err != nil {
		return false, err
	}
	if manifestFile == "" {
		return false, nil
	}
	manifest, err := pm.ReadNativeManifest(manifestFile)
	if err != nil {
		return false, nil
	}
	var rawKind any = "standalone"
	if v, ok := manifest["kind"]; ok {
		rawKind = v
	}
	kind := strings.ToLower(pystr.Strip(pyrepr.Str(rawKind)))
	return kind == "model-provider" || (source == "bundled" && bundledDefaultOnKinds[kind]), nil
}

// Provider names switched on through category config rather than plugins.enabled
func categoryActiveNames(h string) map[string]bool {
	out := map[string]bool{}
	if n := getCurrentMemoryProvider(h); n != "" {
		out[n] = true
	}
	return out
}

// User facing activation state mirroring the manifest gate
func pluginStatus(h string, e PluginEntry, enabled, disabled, active map[string]bool, withDir bool) (string, error) {
	names := map[string]bool{e.Name: true, e.Key: true}
	if intersects(names, disabled) {
		return "disabled", nil
	}
	if intersects(names, enabled) || intersects(names, active) {
		return "enabled", nil
	}
	if withDir {
		on, err := defaultOn(h, e.Dir, e.Source, e.Key)
		if err != nil {
			return "", err
		}
		if on {
			return "enabled", nil
		}
	}
	return "not enabled", nil
}

// Toolset a plugin registers its tools under, from the loaded plugin or its manifest provides_tools
func getPluginToolsetKey(ctx context.Context, h, name string) string {
	m := GetPluginManager(ctx)
	firstToolset := func(toolNames []string) string {
		for _, t := range toolNames {
			if def := tools.Default.Lookup(ctx, m.home, t); def != nil && def.Toolset != "" {
				return def.Toolset
			}
		}
		return ""
	}
	fromLoaded := func() string {
		DiscoverPlugins(ctx, false)
		m.mu.RLock()
		defer m.mu.RUnlock()
		for _, key := range sortedKeys(m.plugins) {
			loaded := m.plugins[key]
			if (loaded.Manifest != nil && loaded.Manifest.Name == name) || key == name {
				return firstToolset(loaded.ToolsRegistered)
			}
		}
		return ""
	}
	fromDisk := func() string {
		user, err := pluginsDir(h)
		if err != nil {
			return ""
		}
		for _, base := range []string{GetBundledPluginsDir(), user} {
			dir := filepath.Join(base, name)
			if st, err := os.Stat(dir); base == "" || err != nil || !st.IsDir() {
				continue
			}
			manifest, err := readManifest(dir)
			if err != nil {
				return ""
			}
			provided, _ := manifest["provides_tools"].([]any)
			names := make([]string, 0, len(provided))
			for _, p := range provided {
				names = append(names, scalarString(p))
			}
			if toolset := firstToolset(names); toolset != "" {
				return toolset
			}
		}
		return ""
	}
	for _, lookup := range []func() string{fromLoaded, fromDisk} {
		if toolset := safeLookup(lookup); toolset != "" {
			return toolset
		}
	}
	return ""
}

// Runs one toolset lookup, a panicking lookup answering empty like the swallowed exception
func safeLookup(lookup func() string) (out string) {
	defer func() {
		if recover() != nil {
			out = ""
		}
	}()
	return lookup()
}

// Toolset key of a plugin, empty without loading anything when no platform has a saved list
func savedListToolsetKey(ctx context.Context, h, name string) (string, error) {
	cfg, err := loadConfig(h)
	if err != nil {
		return "", err
	}
	saved, ok := cfg["platform_toolsets"].(map[string]any)
	if !ok {
		return "", nil
	}
	for _, v := range saved {
		if _, parsed := tools.ParsePlatformToolsetsValue(v); parsed {
			return getPluginToolsetKey(ctx, h, name), nil
		}
	}
	return "", nil
}

// Adds or removes a plugin toolset in every saved platform_toolsets list
func togglePluginToolset(ctx context.Context, h, name string, enable bool, toolsetKey string) error {
	if toolsetKey == "" {
		var err error
		if toolsetKey, err = savedListToolsetKey(ctx, h, name); err != nil {
			return err
		}
	}
	if toolsetKey == "" {
		return nil
	}
	return updateUserConfig(h, func(cfg map[string]any) error {
		platformToolsets := childMap(cfg, "platform_toolsets")
		for _, platform := range sortedKeys(platformToolsets) {
			list, ok := tools.ParsePlatformToolsetsValue(platformToolsets[platform])
			if !ok || enable == slices.Contains(list, toolsetKey) {
				continue
			}
			if enable {
				list = append(list, toolsetKey)
			} else if i := slices.Index(list, toolsetKey); i >= 0 {
				list = slices.Delete(list, i, i+1)
			}
			platformToolsets[platform] = anyList(list)
		}
		return nil
	})
}

// Enables or disables a plugin for the dashboard, loading an enabled plugin live
func DashboardSetAgentPluginEnabled(ctx context.Context, name string, enabled bool) (map[string]any, error) {
	h := home.Dir(ctx)
	key, err := resolveNamedPluginKey(ctx, name)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return map[string]any{"ok": false, "error": fmt.Sprintf("Plugin '%s' is not installed or bundled.", name)}, nil
	}
	toolsetKey := ""
	if !enabled {
		if toolsetKey, err = savedListToolsetKey(ctx, h, key); err != nil {
			return nil, err
		}
	}
	changed, err := activateKey(ctx, h, key, enabled, nil)
	var refused *AdmissionRefused
	if errors.As(err, &refused) {
		return map[string]any{"ok": false, "error": refused.Error(), "name": key, "unchanged": true, "restart_required": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if changed && enabled {
		result := map[string]any{"ok": true, "name": key, "unchanged": false}
		activation, err := activatePluginNow(ctx, key)
		if err != nil {
			return nil, err
		}
		for k, v := range activation {
			result[k] = v
		}
		return result, togglePluginToolset(ctx, h, key, true, "")
	}
	if changed {
		if err := togglePluginToolset(ctx, h, key, false, toolsetKey); err != nil {
			return nil, err
		}
	}
	return map[string]any{"ok": true, "name": key, "unchanged": !changed, "restart_required": changed}, nil
}

// User plugins dir child of that name when it is a directory, else the user install the name resolves to
func userInstalledPluginDir(ctx context.Context, name string) (string, error) {
	dir, err := pluginsDir(home.Dir(ctx))
	if err != nil {
		return "", err
	}
	target, err := sanitizePluginName(name, dir, true)
	if err != nil {
		return "", nil
	}
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target, nil
	}
	return resolvedUserPluginDir(ctx, name)
}

// Dir of the user install a manifest or catalog name resolves to, empty when none
func resolvedUserPluginDir(ctx context.Context, name string) (string, error) {
	entry, err := findPluginEntry(ctx, name)
	if err != nil || entry == nil || (entry.Source != "user" && entry.Source != "git") || entry.Dir == "" {
		return "", err
	}
	if st, err := os.Stat(entry.Dir); err != nil || !st.IsDir() {
		return "", nil
	}
	return entry.Dir, nil
}

// Validates one plugin through runtime discovery and registration, exit 1 under ci when it fails
func CmdPluginDoctor(ctx context.Context, target string, ci bool, out io.Writer) error {
	report, err := doctorPlugin(ctx, target)
	if err != nil {
		return err
	}
	NewConsole(out).Print(report.FormatText())
	if ci && !report.OK {
		return &CommandExit{Code: 1}
	}
	return nil
}
