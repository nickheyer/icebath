package tuiserver

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Mapping under a key of a manifest mapping, empty when absent and an attribute error when not a mapping
func manifestMapping(m map[string]any, key string) (map[string]any, error) {
	v, present := m[key]
	if !present {
		return map[string]any{}, nil
	}
	out, isMap := v.(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(v))
	}
	return out, nil
}

// Per server rows of an installed portable plugin with each server state and its unavailable sentence
func pluginServerRows(ctx context.Context, pluginDir, key string, portable bool, catalogTitles map[string]string) ([]map[string]any, error) {
	if !portable || pluginDir == "" {
		return []map[string]any{}, nil
	}
	pkg, err := plugins.LoadAgentPlugin(pluginDir, pluginDir)
	if err != nil {
		return nil, err
	}
	extensions, err := manifestMapping(pkg.Manifest, "extensions")
	if err != nil {
		return nil, err
	}
	namespace, err := manifestMapping(extensions, "com.nousresearch.hermes")
	if err != nil {
		return nil, err
	}
	declaredRaw, present := namespace["servers"]
	if !present {
		declaredRaw = map[string]any{}
	}
	declared, isMap := declaredRaw.(map[string]any)
	if !isMap {
		return []map[string]any{}, nil
	}
	displayName := ""
	if name := pkg.Manifest["name"]; pyrepr.Truthy(name) {
		displayName = pyrepr.Str(name)
	}
	sidecar, err := plugins.CatalogInstallRecord(ctx, pluginDir)
	if err != nil {
		return nil, err
	}
	if len(sidecar) > 0 {
		catalogName := ""
		if v := sidecar["catalog_name"]; pyrepr.Truthy(v) {
			catalogName = pyrepr.Str(v)
		}
		if title := catalogTitles[catalogName]; title != "" {
			displayName = title
		}
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := []map[string]any{}
	for _, name := range names {
		internalName := plugins.PortableMCPServerName(name)
		if mcp.ServerConnected(ctx, internalName) {
			rows = append(rows, map[string]any{"name": name, "state": "connected", "sentence": ""})
			continue
		}
		state, sentence, known := mcp.ServerUnavailableState(ctx, internalName, displayName)
		if !known {
			rows = append(rows, map[string]any{"name": name, "state": "unknown", "sentence": ""})
			continue
		}
		rows = append(rows, map[string]any{"name": name, "state": state, "sentence": sentence})
	}
	return rows, nil
}

// Command, arguments and url a server config runs
type serverIdent struct {
	command string
	args    []string
	url     string
}

// Whether two server identities run the same thing
func (a serverIdent) same(b serverIdent) bool {
	return a.command == b.command && a.url == b.url && slices.Equal(a.args, b.args)
}

// What a server config runs after environment expansion, its command, arguments and url
func serverIdentity(ctx context.Context, env *tools.Env, cfg any) (serverIdent, error) {
	mapping, isMap := cfg.(map[string]any)
	if !isMap {
		mapping = map[string]any{}
	}
	rendered, err := mcp.InterpolateServerConfig(ctx, env, mapping)
	if err != nil {
		return serverIdent{}, err
	}
	expanded, _ := rendered.(map[string]any)
	text := func(v any) string {
		if pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return ""
	}
	ident := serverIdent{command: text(expanded["command"]), url: text(expanded["url"])}
	args := expanded["args"]
	list, isList := args.([]any)
	switch {
	case isList && len(list) > 0:
		for _, a := range list {
			ident.args = append(ident.args, pyrepr.Str(a))
		}
	case pyrepr.Truthy(args):
		ident.args = []string{pyrepr.Str(args)}
	default:
		ident.args = []string{"()"}
	}
	return ident, nil
}

// Tools each portable package's own MCP servers registered, live or cached, skipping names native config claims for another server
func portableServerTools(ctx context.Context, manager *plugins.Manager, native map[string]any) (map[string]map[string]bool, error) {
	env := configEnv(ctx)
	declared := manager.GetPortableMCPServers()
	listed := map[string]map[string]bool{}
	for serverName, owner := range manager.GetPortableMCPServerPlugins() {
		names := listed[owner]
		if names == nil {
			names = map[string]bool{}
			listed[owner] = names
		}
		if nativeCfg, claimed := native[serverName]; claimed {
			mine, err := serverIdentity(ctx, env, nativeCfg)
			if err != nil {
				return nil, err
			}
			var declaredCfg any
			if cfg, found := declared[serverName]; found {
				declaredCfg = cfg
			}
			theirs, err := serverIdentity(ctx, env, declaredCfg)
			if err != nil {
				return nil, err
			}
			if !mine.same(theirs) {
				continue
			}
		}
		for _, name := range mcp.ServerToolNames(ctx, serverName) {
			names[name] = true
		}
	}
	return listed, nil
}

// Exact tool names each loaded plugin's own code registered by manifest key, a name two packages list belonging to neither
func pluginToolOwnership(ctx context.Context) (map[string][]string, error) {
	plugins.DiscoverPlugins(ctx, false)
	manager := plugins.GetPluginManager(ctx)
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	native, _ := cfg["mcp_servers"].(map[string]any)
	if native == nil {
		native = map[string]any{}
	}
	builtin := map[string]bool{}
	for _, name := range tools.CoreTools {
		builtin[name] = true
	}
	for _, entry := range tools.Toolsets {
		for _, name := range entry.Tools {
			builtin[name] = true
		}
	}
	listed := map[string]map[string]bool{}
	for _, loaded := range manager.LoadedPluginsInOrder() {
		names := map[string]bool{}
		for _, name := range loaded.Plugin.ToolsRegistered {
			if !builtin[name] {
				names[name] = true
			}
		}
		listed[loaded.Key] = names
	}
	portable, err := portableServerTools(ctx, manager, native)
	if err != nil {
		return nil, err
	}
	for owner, names := range portable {
		if listed[owner] == nil {
			listed[owner] = map[string]bool{}
		}
		for name := range names {
			listed[owner][name] = true
		}
	}
	seen := map[string]int{}
	for _, names := range listed {
		for name := range names {
			seen[name]++
		}
	}
	out := map[string][]string{}
	for key, names := range listed {
		owned := []string{}
		for name := range names {
			if seen[name] == 1 {
				owned = append(owned, name)
			}
		}
		sort.Strings(owned)
		out[key] = owned
	}
	return out, nil
}
