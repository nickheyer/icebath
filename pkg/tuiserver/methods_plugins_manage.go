package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/desktopplugins"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// One plugins.manage action handler
type pluginsAction func(ctx context.Context, rid any, params map[string]any) (map[string]any, error)

// Every installed and bundled plugin row the Plugins hub lists, sorted by name, version, description, source, directory and key
func pluginRows(ctx context.Context) ([]map[string]any, error) {
	h := home.Dir(ctx)
	enabled, disabled := plugins.EnabledPluginSet(ctx), plugins.DisabledPluginSet(ctx)
	pins, versions, titles := plugins.CatalogRowsMaps(ctx)
	refPins, err := plugins.ReadInstallMetadata(h)
	if err != nil {
		return nil, err
	}
	gitUpdates := plugins.CachedGitUpdates(ctx, refPins)
	active := plugins.CategoryActiveNames(ctx)
	ownedTools, err := pluginToolOwnership(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := plugins.DiscoverAllPlugins(h)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		for _, pair := range [][2]string{{a.Name, b.Name}, {a.Version, b.Version}, {a.Description, b.Description},
			{a.Source, b.Source}, {a.Dir, b.Dir}, {a.Key, b.Key}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	out := []map[string]any{}
	for _, e := range entries {
		status, err := plugins.ListedPluginStatus(ctx, e, enabled, disabled, active)
		if err != nil {
			return nil, err
		}
		portable := plugins.IsPortablePluginDir(e.Dir)
		servers, err := pluginServerRows(ctx, e.Dir, e.Key, portable, titles)
		if err != nil {
			return nil, err
		}
		owned := ownedTools[e.Key]
		if owned == nil {
			owned = []string{}
		}
		row := map[string]any{
			"name": e.Name, "key": e.Key, "version": e.Version, "description": e.Description,
			"source": e.Source, "status": status, "portable": portable, "install_dir": e.Dir,
			"has_desktop_half": e.Dir != "" && desktopplugins.DesktopSource(e.Dir) != "",
			"settings_schema":  plugins.PluginSettingsFields(ctx, h, e.Key, e.Dir),
			"servers":          servers,
			"tools":            owned,
		}
		fields, err := plugins.CatalogRowFields(h, e.Dir, pins, versions)
		if err != nil {
			return nil, err
		}
		for k, v := range fields {
			row[k] = v
		}
		if e.Dir != "" {
			if target := gitUpdates[filepath.Base(e.Dir)]; target != "" {
				row["update_available"], row["update_target"] = true, target
			}
		}
		sha, err := plugins.PinnedRevision(ctx, e.Name, refPins)
		if err != nil {
			return nil, err
		}
		if sha != "" {
			row["pinned_sha"] = sha
		}
		out = append(out, row)
	}
	return out, nil
}

// Latest load summaries by plugin name and key, filled by one listener per plugin manager scope
var (
	pluginActivationsMu        sync.Mutex
	pluginActivations          = map[string]map[string]any{}
	pluginActivationSubscribed = map[string]bool{}
)

// Subscribes this server to plugin load summaries once per plugin manager so results report what a load activated
func ensurePluginActivationListener(ctx context.Context) error {
	manager := plugins.GetPluginManager(ctx)
	pluginActivationsMu.Lock()
	if pluginActivationSubscribed[manager.ScopeKey()] {
		pluginActivationsMu.Unlock()
		return nil
	}
	pluginActivationSubscribed[manager.ScopeKey()] = true
	pluginActivationsMu.Unlock()
	_, err := manager.OnPluginLoaded(func(summaries []map[string]any) {
		pluginActivationsMu.Lock()
		defer pluginActivationsMu.Unlock()
		for _, entry := range summaries {
			pluginActivations[pyrepr.Str(entry["name"])] = entry
			pluginActivations[pyrepr.Str(entry["key"])] = entry
		}
	})
	return err
}

// Result with activation filled from this server's listener when the core returned none
func withActivation(result map[string]any, name string) map[string]any {
	if pyrepr.Truthy(result["activation"]) {
		return result
	}
	pluginActivationsMu.Lock()
	defer pluginActivationsMu.Unlock()
	for _, key := range []any{name, result["plugin_name"], result["name"]} {
		if !pyrepr.Truthy(key) {
			continue
		}
		if entry, found := pluginActivations[pyrepr.Str(key)]; found {
			result["activation"] = entry
			break
		}
	}
	return result
}

// First truthy param among the keys, stripped
func firstParam(params map[string]any, keys ...string) string {
	for _, key := range keys {
		if v := params[key]; pyrepr.Truthy(v) {
			return pystr.Strip(strOr(v))
		}
	}
	return ""
}

// Plugin rows with the user and bundled counts
func pluginsListRows(ctx context.Context, rid any, _ map[string]any) (map[string]any, error) {
	rows, err := pluginRows(ctx)
	if err != nil {
		return nil, err
	}
	userCount := 0
	for _, r := range rows {
		if r["source"] != "bundled" {
			userCount++
		}
	}
	return ok(rid, map[string]any{"plugins": rows, "user_count": userCount, "bundled_count": len(rows) - userCount}), nil
}

// Enables or disables one plugin by key or name and answers with its canonical key and listing row
func pluginsToggle(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	ident := firstParam(params, "key", "name")
	if ident == "" {
		return rpcErr(rid, 4019, "plugins.toggle requires a 'key' or 'name'", nil), nil
	}
	if err := ensurePluginActivationListener(ctx); err != nil {
		return nil, err
	}
	result, err := plugins.DashboardSetAgentPluginEnabled(ctx, ident, pyrepr.Truthy(params["enable"]))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 5026, errorOr(result, "toggle failed"), nil), nil
	}
	key := ident
	if pyrepr.Truthy(result["name"]) {
		key = pyrepr.Str(result["name"])
	}
	rows, err := pluginRows(ctx)
	if err != nil {
		return nil, err
	}
	var row any
	for _, r := range rows {
		if r["key"] == key || r["name"] == key {
			row = r
			break
		}
	}
	return ok(rid, withActivation(map[string]any{
		"ok": true, "unchanged": pyrepr.Truthy(result["unchanged"]),
		"restart_required": pyrepr.Truthy(result["restart_required"]),
		"gateway_reloaded": pyrepr.Truthy(result["gateway_reloaded"]), "activation": result["activation"],
		"name": key, "plugin": row}, key)), nil
}

// Error text of a failed core result, the fallback when it carries none
func errorOr(result map[string]any, fallback string) string {
	if pyrepr.Truthy(result["error"]) {
		return pyrepr.Str(result["error"])
	}
	return fallback
}

// Structured marker of an install refused because the source left the reviewed commit, nil otherwise
func sourceMovedData(result map[string]any) any {
	if pyrepr.Truthy(result["source_moved"]) {
		return map[string]any{"reason": "source_moved"}
	}
	return nil
}

// Clones a git source or a curated catalog entry at its pin and installs it
func pluginsInstall(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	ident := firstParam(params, "identifier", "repo")
	catalogName := strArg(params, "catalog_name")
	if ident == "" && catalogName == "" {
		return rpcErr(rid, 4019, "plugins.install requires 'identifier', 'repo', or 'catalog_name'", nil), nil
	}
	if err := ensurePluginActivationListener(ctx); err != nil {
		return nil, err
	}
	enable := true
	if v, present := params["enable"]; present {
		enable = pyrepr.Truthy(v)
	}
	result, err := plugins.DashboardInstallPlugin(ctx, plugins.DashboardInstallOptions{
		Identifier: ident, Force: pyrepr.Truthy(params["force"]), Enable: enable, CatalogName: catalogName,
		Ref: strArg(params, "ref"), InspectedSha: strArg(params, "inspected_sha")})
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 5026, errorOr(result, "install failed"), sourceMovedData(result)), nil
	}
	if pyrepr.Truthy(result["enabled"]) {
		return ok(rid, withActivation(result, strOr(result["plugin_name"]))), nil
	}
	return ok(rid, result), nil
}

// Read only pre install check of which halves a source ships
func pluginsInspect(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	ident := firstParam(params, "identifier", "repo")
	catalogName := strArg(params, "catalog_name")
	if ident == "" && catalogName == "" {
		return rpcErr(rid, 4019, "plugins.inspect requires 'identifier', 'repo', or 'catalog_name'", nil), nil
	}
	result, err := plugins.InspectPluginSource(ctx, ident, catalogName)
	if err != nil {
		return nil, err
	}
	return ok(rid, result), nil
}

// Installs a source's desktop half into desktop-plugins on the host serving this client
func pluginsInstallDesktop(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	ident := firstParam(params, "identifier", "repo")
	catalogName := strArg(params, "catalog_name")
	if ident == "" && catalogName == "" {
		return rpcErr(rid, 4019, "plugins.manage install_desktop requires 'identifier', 'repo', or 'catalog_name'", nil), nil
	}
	ref, isText := params["ref"].(string)
	if !isText && pyrepr.Truthy(params["ref"]) {
		return nil, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(params["ref"]))
	}
	result, err := desktopplugins.InstallDesktopPlugin(ctx, ident, ref, catalogName, pyrepr.Truthy(params["force"]))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 5026, errorOr(result, "desktop install failed"), sourceMovedData(result)), nil
	}
	return ok(rid, result), nil
}

// Deletes one installed desktop plugin folder from this host
func pluginsRemoveDesktop(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	name := firstParam(params, "name", "key")
	if name == "" {
		return rpcErr(rid, 4019, "plugins.manage remove_desktop requires a 'name'", nil), nil
	}
	result, err := desktopplugins.RemoveDesktopPlugin(name, pyrepr.Truthy(params["force"]))
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 5026, errorOr(result, "desktop remove failed"), nil), nil
	}
	return ok(rid, result), nil
}

// Re pins a catalog install or pulls a git install, answering a consent or scan refusal with what the client shows
func pluginsUpdate(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	name := firstParam(params, "name")
	if name == "" {
		return rpcErr(rid, 4019, "plugins.update requires a 'name'", nil), nil
	}
	if err := ensurePluginActivationListener(ctx); err != nil {
		return nil, err
	}
	result, err := plugins.DashboardUpdateUserPlugin(ctx, name, pyrepr.Truthy(params["accept_capabilities"]), pyrepr.Truthy(params["force"]))
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(result["consent_required"]) || pyrepr.Truthy(result["scan_blocked"]) {
		refusal := map[string]any{}
		for _, k := range []string{"ok", "name", "sha", "error", "consent_required", "delta", "delta_lines",
			"scan_blocked", "scan_verdict", "scan_findings"} {
			if v, present := result[k]; present {
				refusal[k] = v
			}
		}
		return ok(rid, refusal), nil
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 4021, errorOr(result, "update failed"), nil), nil
	}
	resultName := name
	if pyrepr.Truthy(result["name"]) {
		resultName = pyrepr.Str(result["name"])
	}
	warnings := []any{}
	if list, isList := result["warnings"].([]any); isList {
		warnings = append(warnings, list...)
	} else if list, isList := result["warnings"].([]string); isList {
		for _, w := range list {
			warnings = append(warnings, w)
		}
	}
	payload := map[string]any{"ok": true, "unchanged": pyrepr.Truthy(result["unchanged"]), "name": resultName, "warnings": warnings}
	for _, k := range []string{"sha", "gateway_reloaded", "activation", "restart_required"} {
		if v, present := result[k]; present {
			payload[k] = v
		}
	}
	if pyrepr.Truthy(result["unchanged"]) {
		return ok(rid, payload), nil
	}
	return ok(rid, withActivation(payload, resultName)), nil
}

// Uninstalls a user install by name through the same core as the CLI and the dashboard
func pluginsRemove(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	name := firstParam(params, "name")
	if name == "" {
		return rpcErr(rid, 4019, "plugins.remove requires a 'name'", nil), nil
	}
	result, err := plugins.DashboardRemoveUserPlugin(ctx, name)
	if err != nil {
		return nil, err
	}
	if !pyrepr.Truthy(result["ok"]) {
		return rpcErr(rid, 5026, errorOr(result, "remove failed"), nil), nil
	}
	return ok(rid, result), nil
}

// Writes manifest declared settings, tells the desktop half and reconnects package MCP servers whose inputs changed
func pluginsSettings(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	key := firstParam(params, "key", "name")
	values, isMap := params["values"].(map[string]any)
	if key == "" || !isMap {
		return rpcErr(rid, 4019, "plugins.settings requires a 'key' and a 'values' mapping", nil), nil
	}
	h := home.Dir(ctx)
	found, err := plugins.FindPluginEntry(h, key)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return rpcErr(rid, 4020, fmt.Sprintf("plugin '%s' not found", key), nil), nil
	}
	canonical, pluginDir := found.Key, found.Dir
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	edits := make([]plugins.PluginSettingValue, 0, len(names))
	for _, name := range names {
		edits = append(edits, plugins.PluginSettingValue{Key: name, Value: values[name]})
	}
	written, err := plugins.SavePluginSettings(ctx, h, canonical, pluginDir, edits)
	if err != nil {
		var refused *plugins.PluginSettingsValueError
		var denied *plugins.PluginPermissionError
		if errors.As(err, &refused) || errors.As(err, &denied) {
			return rpcErr(rid, 4021, err.Error(), nil), nil
		}
		return nil, err
	}
	rows, err := pluginRows(ctx)
	if err != nil {
		return nil, err
	}
	var row any
	for _, r := range rows {
		if r["key"] == canonical {
			row = r
			break
		}
	}
	if plugins.IsPluginEventID(canonical) {
		if err := plugins.BroadcastPluginEvent(canonical, "settings.changed", map[string]any{"written": written}); err != nil {
			return nil, err
		}
	}
	if pluginDir != "" && plugins.IsPortablePluginDir(pluginDir) {
		servers, err := portableServerNames(pluginDir)
		if err != nil {
			return nil, err
		}
		if len(servers) > 0 {
			thread := "plugin-settings-mcp-" + canonical
			memory.SpawnContextThread(ctx, thread, func(ctx context.Context) {
				if failure := catchPanic(func() {
					_, err := mcp.Restart(ctx, configEnv(ctx), servers)
					panicIf(err)
				}); failure != nil {
					logger().Warn(fmt.Sprintf("Exception in thread %s: %s", thread, panicMessage(failure)))
				}
			}).Start()
		}
	}
	return ok(rid, map[string]any{"ok": true, "name": canonical, "written": written, "plugin": row}), nil
}

// Internal names of the MCP servers a portable package declares, sorted
func portableServerNames(pluginDir string) ([]string, error) {
	pkg, err := plugins.LoadAgentPlugin(pluginDir, pluginDir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for server := range pkg.MCPServers {
		set[plugins.PortableMCPServerName(server)] = true
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Catalog plugins curated for the onboarding card that this OS runs, each with its app state
func pluginsOnboarding(ctx context.Context, rid any, _ map[string]any) (map[string]any, error) {
	entries, err := plugins.OnboardingEntries(ctx)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []plugins.OnboardingEntry{}
	}
	return ok(rid, map[string]any{"onboarding": entries}), nil
}

// Named catalog plugins this OS runs in the order asked, each with its app state and disclosure
func pluginsPresence(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	var names []string
	if raw := params["names"]; pyrepr.Truthy(raw) {
		switch v := raw.(type) {
		case []any:
			for _, n := range v {
				if text, isText := n.(string); isText {
					names = append(names, text)
				}
			}
		case string:
			for _, r := range v {
				names = append(names, string(r))
			}
		case map[string]any:
			for k := range v {
				names = append(names, k)
			}
			sort.Strings(names)
		default:
			return nil, fmt.Errorf("'%s' object is not iterable", pyrepr.TypeName(raw))
		}
	}
	found, err := plugins.PresenceFor(ctx, names)
	if err != nil {
		return nil, err
	}
	if found == nil {
		found = []plugins.NamedPresence{}
	}
	return ok(rid, map[string]any{"presence": found}), nil
}

// Action table of plugins.manage
var pluginsActions = map[string]pluginsAction{
	"list": pluginsListRows, "onboarding": pluginsOnboarding, "presence": pluginsPresence,
	"toggle": pluginsToggle, "install": pluginsInstall, "inspect": pluginsInspect,
	"install_desktop": pluginsInstallDesktop, "remove_desktop": pluginsRemoveDesktop,
	"update": pluginsUpdate, "remove": pluginsRemove, "settings": pluginsSettings,
}

// Dispatches the action param, list by default, through the table, an unknown action answering 4017
func runAction[A any](rid any, params map[string]any, table map[string]A, label string, invoke func(A) (map[string]any, error)) (map[string]any, error) {
	action, present := params["action"]
	if !present {
		action = "list"
	}
	switch action.(type) {
	case []any, map[string]any:
		return nil, fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(action))
	}
	name, isText := action.(string)
	handler, known := table[name]
	if !isText || !known {
		return rpcErr(rid, 4017, fmt.Sprintf("unknown %s action: %s", label, pyrepr.Str(action)), nil), nil
	}
	return invoke(handler)
}

// Plugins hub backend listing, toggling, installing, inspecting, updating, removing and configuring plugins
func pluginsManage(ctx context.Context, rid any, params map[string]any, _ *tuigateway.Session) (map[string]any, error) {
	return runAction(rid, params, pluginsActions, "plugins", func(h pluginsAction) (map[string]any, error) { return h(ctx, rid, params) })
}

func init() {
	scopedRPC(toolsRPCRegistry, "plugins.manage", 5026, rpcShape{propagateResolve: true}, pluginsManage)
}
