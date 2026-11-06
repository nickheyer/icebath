package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var goLiveMu sync.Mutex

var gatewayTransformHooks = map[string]bool{
	"transform_llm_output": true, "transform_tool_result": true, "transform_terminal_output": true,
	"pre_gateway_dispatch": true, "gateway_platform_event": true, "pre_command": true,
}

const (
	hostRoleServe        = "serve"
	hostRoleDesktopServe = "desktop-serve"
	serveBackendTimeout  = 60 * time.Second
)

var restartLines = map[string]string{
	"gateway": "  icebath gateway restart",
	"backend": "  quit and reopen the Icebath desktop app (or restart icebath dashboard)",
}

// Sorted copy of a string list, duplicates kept
func sortedStrings(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return out
}

// What one loaded plugin registered, split into live now and deferred kinds, with missing dependencies
func pluginActivationSummary(ctx context.Context, m *Manager, pluginKey string) map[string]any {
	m.mu.RLock()
	loaded := m.plugins[pluginKey]
	var manifest *Manifest
	if loaded != nil {
		manifest = loaded.Manifest
	}
	name := pluginKey
	if manifest != nil && manifest.Name != "" {
		name = manifest.Name
	}
	kinds := map[string][]string{}
	for _, r := range m.ownershipLedger[pluginKey] {
		if r.Active() {
			kinds[r.Kind] = append(kinds[r.Kind], r.Key)
		}
	}
	now := map[string]any{}
	if len(kinds["command"]) > 0 {
		now["gateway_commands"] = anyList(sortedStrings(kinds["command"]))
	}
	transforms, others := map[string]bool{}, map[string]bool{}
	for _, h := range kinds["hook"] {
		if gatewayTransformHooks[h] {
			transforms[h] = true
		} else {
			others[h] = true
		}
	}
	if len(transforms) > 0 {
		now["gateway_transforms"] = anyList(sortedSet(transforms))
	}
	if len(others) > 0 {
		now["hooks"] = anyList(sortedSet(others))
	}
	var callbacks []string
	for _, platform := range sortedKeys(m.platformHandlerFactories) {
		for _, f := range m.platformHandlerFactories[platform] {
			if f.Plugin == name {
				callbacks = append(callbacks, platform)
				break
			}
		}
	}
	for _, a := range kinds["slack_action_handler"] {
		callbacks = append(callbacks, "slack:"+a)
	}
	if len(callbacks) > 0 {
		now["callbacks"] = anyList(callbacks)
	}
	if len(kinds["locale"]) > 0 {
		now["locales"] = anyList(sortedStrings(kinds["locale"]))
	}
	deferred := map[string]any{}
	tools := map[string]bool{}
	for _, t := range kinds["tool"] {
		tools[t] = true
	}
	if loaded != nil {
		for _, t := range loaded.ToolsRegistered {
			tools[t] = true
		}
	}
	if manifest != nil {
		for _, t := range manifest.ProvidesTools {
			tools[t] = true
		}
	}
	if len(tools) > 0 {
		deferred["tools"] = anyList(sortedSet(tools))
	}
	if len(kinds["system_prompt_section"]) > 0 {
		deferred["prompt"] = anyList(sortedStrings(kinds["system_prompt_section"]))
	}
	servers := map[string]bool{}
	for _, s := range kinds["portable_mcp"] {
		servers[s] = true
	}
	for s, owner := range m.portableMCPServerPlugins {
		if owner == pluginKey {
			servers[s] = true
		}
	}
	if len(servers) > 0 {
		deferred["mcp_servers"] = anyList(sortedSet(servers))
	}
	m.mu.RUnlock()
	summary := map[string]any{"name": name, "key": pluginKey, "activated_now": now, "deferred": deferred}
	if manifest != nil {
		if missing := missingPythonDependencies(ctx, manifest); len(missing) > 0 {
			summary["missing_dependencies"] = anyList(missing)
		}
	}
	return summary
}

// Snapshot of the plugin keys with their loaded records in key order
func (m *Manager) loadedSnapshot() ([]string, []*LoadedPlugin) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := sortedKeys(m.plugins)
	loaded := make([]*LoadedPlugin, len(keys))
	for i, k := range keys {
		loaded[i] = m.plugins[k]
	}
	return keys, loaded
}

// One summary per loaded plugin that is neither a deferred platform nor errored, the plugin loaded payload
func ActivationSummaries(ctx context.Context, m *Manager) []map[string]any {
	out := []map[string]any{}
	keys, loaded := m.loadedSnapshot()
	for i, key := range keys {
		if loaded[i].Deferred || loaded[i].Error != "" {
			continue
		}
		out = append(out, pluginActivationSummary(ctx, m, key))
	}
	return out
}

// Plugins that failed to load while a declared Python dependency is not importable in this process
func DependencyBlockedSummaries(ctx context.Context, m *Manager) []map[string]any {
	out := []map[string]any{}
	keys, loaded := m.loadedSnapshot()
	for i, key := range keys {
		manifest := loaded[i].Manifest
		if loaded[i].Error == "" || manifest == nil {
			continue
		}
		if missing := missingPythonDependencies(ctx, manifest); len(missing) > 0 {
			name := key
			if manifest.Name != "" {
				name = manifest.Name
			}
			out = append(out, map[string]any{"name": name, "key": key, "activated_now": map[string]any{},
				"deferred": map[string]any{}, "missing_dependencies": anyList(missing)})
		}
	}
	return out
}

// Dict of a truthy value or an empty dict, an AttributeError for any other truthy value
func pyDictOr(v any) (map[string]any, error) {
	if !pyrepr.Truthy(v) {
		return map[string]any{}, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(v))
}

// Items iterating a truthy value yields, none for a falsy one, a TypeError for a non iterable
func pyItemsOr(v any) ([]any, error) {
	if !pyrepr.Truthy(v) {
		return nil, nil
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case []string:
		return anyList(x), nil
	case []map[string]any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out, nil
	case string:
		var out []any
		for _, r := range x {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		return anyList(sortedKeys(x)), nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not iterable", pyrepr.TypeName(v))
}

// Length of a truthy value or zero, a TypeError for a value without a length
func pyLenOr(v any) (int, error) {
	if !pyrepr.Truthy(v) {
		return 0, nil
	}
	switch x := v.(type) {
	case string:
		return len([]rune(x)), nil
	case map[string]any:
		return len(x), nil
	}
	items, err := pyItemsOr(v)
	if err != nil {
		return 0, fmt.Errorf("TypeError: object of type '%s' has no len()", pyrepr.TypeName(v))
	}
	return len(items), nil
}

// String items of a list, a TypeError naming the first item that is not a string
func pyStrItems(v any) ([]string, error) {
	items, err := pyItemsOr(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("TypeError: sequence item %d: expected str instance, %s found", len(out), pyrepr.TypeName(item))
		}
		out = append(out, s)
	}
	return out, nil
}

// Summary for a manifest name, canonical key or bare key leaf, nil when none matches
func findActivation(summaries any, name string) (map[string]any, error) {
	entries, err := pyItemsOr(summaries)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(e))
		}
		key := ""
		if k := entry["key"]; pyrepr.Truthy(k) {
			key = pyrepr.Str(k)
		}
		leaf := key[strings.LastIndex(key, "/")+1:]
		if n, isStr := entry["name"].(string); (isStr && n == name) || key == name || leaf == name {
			return entry, nil
		}
	}
	return nil, nil
}

// Summaries as the generic list findActivation walks
func summaryItems(summaries []map[string]any) []any {
	out := make([]any, len(summaries))
	for i, s := range summaries {
		out[i] = s
	}
	return out
}

// Loads the plugin here, hands it to open chats and nudges the running gateway, reporting what applies now
func activatePluginNow(ctx context.Context, name string) (map[string]any, error) {
	return activatePluginNowFrom(ctx, name, true)
}

// Activation from this process, or through the running serve backend when the caller is another process
func activatePluginNowFrom(ctx context.Context, name string, inProcess bool) (map[string]any, error) {
	var activation any
	if inProcess {
		live, err := LoadAndGoLive(ctx, name)
		if err != nil {
			return nil, err
		}
		if live != nil {
			activation = live
		}
	} else {
		backend, err := pyDictOr(notifyServeBackend(ctx, name, home.Dir(ctx)))
		if err != nil {
			return nil, err
		}
		activation = backend["activation"]
	}
	answer := reloadRunningGateway(ctx)
	reloaded := answer != nil && pyrepr.Truthy(answer["reloaded"])
	var gatewayActivation map[string]any
	if reloaded {
		found, err := findActivation(answer["activations"], name)
		if err != nil {
			return nil, err
		}
		gatewayActivation = found
	}
	gaps := [2][]string{}
	for i, source := range []any{gatewayActivation, activation} {
		dict, err := pyDictOr(source)
		if err != nil {
			return nil, err
		}
		deps, err := pyStrItems(dict["missing_dependencies"])
		if err != nil {
			return nil, err
		}
		gaps[i] = deps
	}
	if reloaded && activation == nil && gatewayActivation != nil {
		activation = gatewayActivation
	}
	seen := map[string]bool{}
	for _, deps := range gaps {
		for _, d := range deps {
			seen[d] = true
		}
	}
	missing := sortedSet(seen)
	if len(missing) > 0 {
		base := map[string]any{"name": name, "key": name}
		if pyrepr.Truthy(activation) {
			dict, err := pyDictOr(activation)
			if err != nil {
				return nil, fmt.Errorf("TypeError: '%s' object is not a mapping", pyrepr.TypeName(activation))
			}
			base = dict
		}
		merged := make(map[string]any, len(base)+2)
		for k, v := range base {
			merged[k] = v
		}
		merged["missing_dependencies"] = anyList(missing)
		missingIn := []any{}
		for i, process := range []string{"gateway", "backend"} {
			if len(gaps[i]) > 0 {
				missingIn = append(missingIn, process)
			}
		}
		merged["missing_in"] = missingIn
		activation = merged
	}
	return map[string]any{"gateway_reloaded": reloaded, "activation": activation,
		"restart_required": !reloaded || len(missing) > 0}, nil
}

// Asks the gateway serving this profile, else the multiplexer serving it, to rediscover plugins now
func reloadRunningGateway(ctx context.Context) map[string]any {
	v, err := guardedValue(func() (any, error) {
		s := installedServices()
		if s == nil || s.ReloadGatewayPlugins == nil {
			return nil, &ServiceMissingError{Name: "ReloadGatewayPlugins"}
		}
		h := home.Dir(ctx)
		answer, err := s.ReloadGatewayPlugins(ctx, h, "")
		if err != nil || answer != nil {
			return answer, err
		}
		root := home.Root()
		resolvedRoot, err := pypath.Resolve(root)
		if err != nil {
			return nil, err
		}
		resolvedHome, err := pypath.Resolve(h)
		if err != nil {
			return nil, err
		}
		if resolvedRoot != resolvedHome {
			if answer, err = s.ReloadGatewayPlugins(ctx, root, h); err != nil || answer != nil {
				return answer, err
			}
		}
		return nil, nil
	})
	if err != nil {
		logDebug("gateway reload-plugins nudge failed", "error", err)
		return nil
	}
	answer, _ := v.(map[string]any)
	return answer
}

// The sentence printed after a disable, saying whether the running gateway unloaded the plugin
func gatewayReloadHint(answer map[string]any, name string) string {
	if len(answer) > 0 && pyrepr.Truthy(answer["reloaded"]) {
		return "The running gateway reloaded its plugins and unloaded " + name + "; platform hooks it already " +
			"wired up stay until a gateway restart (icebath gateway restart)."
	}
	if answer != nil {
		reason := "no reason given"
		if e := answer["error"]; pyrepr.Truthy(e) {
			reason = pyrepr.Str(e)
		}
		return "The running gateway refused to reload its plugins: " + reason + ".\n" +
			"Only the saved config changed; restart the gateway serving this profile to apply it:\n" +
			"  icebath gateway restart"
	}
	return "No running gateway answered. If one is running, restart it for this to take effect:\n" +
		"  icebath gateway restart"
}

// Rediscovers plugins here one go live at a time and makes the named plugin live in open chats
func LoadAndGoLive(ctx context.Context, name string) (map[string]any, error) {
	goLiveMu.Lock()
	defer goLiveMu.Unlock()
	return goLive(ctx, name)
}

// Forced rediscovery, MCP connection, skills and the open chat refresh behind LoadAndGoLive
func goLive(ctx context.Context, name string) (map[string]any, error) {
	var activation, blocked map[string]any
	var portable map[string]map[string]any
	var skills []any
	_, err := guardedValue(func() (any, error) {
		joinBackgroundDiscovery(ctx)
		m := GetPluginManager(ctx)
		lctx, unlock := m.lockDiscovery(ctx)
		defer unlock()
		m.DiscoverAndLoad(lctx, true)
		var err error
		if activation, err = findActivation(summaryItems(ActivationSummaries(lctx, m)), name); err != nil {
			return nil, err
		}
		if activation == nil {
			if blocked, err = findActivation(summaryItems(DependencyBlockedSummaries(lctx, m)), name); err != nil {
				return nil, err
			}
		}
		portable = m.GetPortableMCPServers()
		if activation != nil {
			skills = pluginSkills(lctx, pyrepr.Str(activation["key"]))
		}
		return nil, nil
	})
	if err != nil {
		logDebug(fmt.Sprintf("in-process plugin reload after change to %s failed", pyrepr.Repr(name)), "error", err)
		return nil, nil
	}
	if activation == nil {
		return blocked, nil
	}
	servers := connectPluginMCP(ctx, activation, portable)
	if skills == nil {
		skills = []any{}
	}
	activation["live_now"] = map[string]any{"mcp_servers": servers, "skills": skills}
	deferred := map[string]any{}
	if d, ok := activation["deferred"].(map[string]any); ok {
		for k, v := range d {
			if k != "mcp_servers" {
				deferred[k] = v
			}
		}
	}
	activation["deferred"] = deferred
	note, err := liveNotice(activation)
	if err != nil {
		return nil, err
	}
	if s := installedServices(); s != nil && s.RefreshPluginSessions != nil && (len(servers) > 0 || note != "") {
		key := pyrepr.Str(activation["key"])
		if err := callGuarded(func() error { return s.RefreshPluginSessions(ctx, home.Dir(ctx), key, note) }); err != nil {
			logWarn(fmt.Sprintf("open chats were not refreshed after activating %s", pyrepr.Repr(name)), "error", err)
		}
	}
	return activation, nil
}

// Host owner record, else the Desktop child record, that names a port and a consistent token
func serveBackendRecord(hr HostRendezvous) (*HostRecord, error) {
	for _, role := range []string{hostRoleServe, hostRoleDesktopServe} {
		record, err := hr.ReadRecord(role)
		if err != nil {
			return nil, err
		}
		if record != nil && record.Port != 0 && hr.RecordTokenIsConsistent(record) {
			return record, nil
		}
	}
	return nil, nil
}

// Asks the running dashboard or Desktop backend to take the plugin live in a home, nil when none answers
func notifyServeBackend(ctx context.Context, name, h string) any {
	v, err := guardedValue(func() (any, error) {
		s := installedServices()
		if s == nil || s.HostRendezvous == nil {
			return nil, &ServiceMissingError{Name: "HostRendezvous"}
		}
		record, err := serveBackendRecord(s.HostRendezvous)
		if err != nil || record == nil {
			return nil, err
		}
		token, err := s.HostRendezvous.ReadToken(record.Role)
		if err != nil {
			return nil, err
		}
		body, err := pyjson.Dumps(map[string]any{"name": name, "home": h})
		if err != nil {
			return nil, err
		}
		url := "http://" + pyurl.FormatURLHost(s.HostRendezvous.DialHost(record)) + ":" + strconv.Itoa(record.Port) +
			"/api/dashboard/agent-plugins/activate"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(body)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Icebath-Session-Token", token)
		client := &http.Client{Timeout: serveBackendTimeout, Transport: &http.Transport{Proxy: pyurl.UrlopenProxy}}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		var out any
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		return out, nil
	})
	if err != nil {
		logDebug(fmt.Sprintf("serve backend activation for %s failed", pyrepr.Repr(name)), "error", err)
		return nil
	}
	return v
}

// Reports whether a gateway runs for this profile or its root, or a gateway service is installed
func gatewayPresent(ctx context.Context) (bool, error) {
	s := installedServices()
	if s == nil || s.GatewayRunningPid == nil {
		return false, &ServiceMissingError{Name: "GatewayRunningPid"}
	}
	if s.InstalledGatewayServiceKind == nil {
		return false, &ServiceMissingError{Name: "InstalledGatewayServiceKind"}
	}
	homes := map[string]bool{}
	for _, h := range []string{home.Dir(ctx), home.Root()} {
		resolved, err := pypath.Resolve(h)
		if err != nil {
			return false, err
		}
		homes[resolved] = true
	}
	for _, h := range sortedSet(homes) {
		pid, ok, err := s.GatewayRunningPid(filepath.Join(h, "gateway.pid"), false)
		if err != nil {
			return false, err
		}
		if ok && pid != 0 {
			return true, nil
		}
	}
	_, installed := s.InstalledGatewayServiceKind(ctx)
	return installed, nil
}

// Restart or repair advice for each process that reported it cannot import the plugin dependencies
func dependencyRestartHint(ctx context.Context, missing, processes []string) (string, error) {
	deps := strings.Join(missing, ", ")
	lines := make([]string, 0, len(processes))
	for _, p := range processes {
		line, ok := restartLines[p]
		if !ok {
			return "", fmt.Errorf("KeyError: %s", pyrepr.Repr(p))
		}
		lines = append(lines, line)
	}
	restart := strings.Join(lines, "\n")
	if installedInSelectedEnvironment(ctx, missing) {
		return "Its Python dependencies (" + deps + ") were installed after the running gateway or dashboard " +
			"started, and a running process never switches dependency environments. Restart them to " +
			"load the plugin:\n" + restart, nil
	}
	return "Its Python dependencies (" + deps + ") are not installed. Run icebath pm repair, then restart:\n" + restart, nil
}

// After a CLI install or enable, takes the plugin live where it can and prints the resulting hint
func reportActivation(ctx context.Context, console *Console, name string) error {
	result, err := activatePluginNowFrom(ctx, name, false)
	if err != nil {
		return err
	}
	hint, err := activationHint(ctx, result)
	if err != nil {
		return err
	}
	if hint != "" {
		console.Print("[dim]" + hint + "[/dim]")
	}
	return nil
}

// One sentence for CLI surfaces from an activation result, empty when there is nothing to say
func activationHint(ctx context.Context, result map[string]any) (string, error) {
	act, err := pyDictOr(result["activation"])
	if err != nil {
		return "", err
	}
	live, err := pyDictOr(act["live_now"])
	if err != nil {
		return "", err
	}
	servers, err := pyItemsOr(live["mcp_servers"])
	if err != nil {
		return "", err
	}
	var connected, failed []map[string]any
	for _, raw := range servers {
		s, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(raw))
		}
		if pyrepr.Truthy(s["connected"]) {
			connected = append(connected, s)
		} else {
			failed = append(failed, s)
		}
	}
	var lines []string
	if len(connected) > 0 || pyrepr.Truthy(live["skills"]) {
		tools := 0
		for _, s := range connected {
			n, err := pyLenOr(s["tools"])
			if err != nil {
				return "", err
			}
			tools += n
		}
		skills, err := pyLenOr(live["skills"])
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("Live in open chats now: %d MCP tools, %d skills.", tools, skills))
	}
	for _, s := range failed {
		serverName, ok := s["name"]
		if !ok {
			return "", fmt.Errorf("KeyError: 'name'")
		}
		reason := "unknown error"
		if e := s["error"]; pyrepr.Truthy(e) {
			reason = pyrepr.Str(e)
		}
		lines = append(lines, "MCP server "+pyrepr.Str(serverName)+" not connected: "+reason)
	}
	if pyrepr.Truthy(act["missing_dependencies"]) {
		missing, err := pyStrItems(act["missing_dependencies"])
		if err != nil {
			return "", err
		}
		processes, err := pyStrItems(act["missing_in"])
		if err != nil {
			return "", err
		}
		hint, err := dependencyRestartHint(ctx, missing, processes)
		if err != nil {
			return "", err
		}
		return strings.Join(append(lines, hint), "\n"), nil
	}
	if !pyrepr.Truthy(result["gateway_reloaded"]) {
		if len(lines) > 0 {
			return strings.Join(lines, "\n"), nil
		}
		present, err := gatewayPresent(ctx)
		if err != nil {
			return "", err
		}
		if !present {
			return "", nil
		}
		return "Restart the gateway for the plugin to take effect:\n  icebath gateway restart", nil
	}
	now, err := pyDictOr(act["activated_now"])
	if err != nil {
		return "", err
	}
	deferred, err := pyDictOr(act["deferred"])
	if err != nil {
		return "", err
	}
	var parts []string
	if len(now) > 0 {
		parts = append(parts, "active in the running gateway now: "+strings.Join(sortedKeys(now), ", "))
	}
	if len(deferred) > 0 {
		labels := map[string]string{"tools": "tools (next session)", "prompt": "system prompt (next session)",
			"mcp_servers": "MCP servers (next session)"}
		var named []string
		for _, k := range sortedKeys(deferred) {
			if label, ok := labels[k]; ok {
				named = append(named, label)
			} else {
				named = append(named, k)
			}
		}
		parts = append(parts, "deferred: "+strings.Join(named, ", "))
	}
	if len(parts) == 0 {
		return strings.Join(append(lines, "Gateway reloaded plugins; nothing of this plugin needs a session or restart."), "\n"), nil
	}
	return strings.Join(append(lines, "Gateway reloaded plugins — "+strings.Join(parts, "; ")+"."), "\n"), nil
}
