package plugins

import (
	"context"
	"fmt"
	"strings"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Installed MCP connector or the error naming the missing service
func mcpConnector() (MCPConnector, error) {
	if s := installedServices(); s != nil && s.MCP != nil {
		return s.MCP, nil
	}
	return nil, &ServiceMissingError{Name: "MCP"}
}

// Connects every portable MCP server the activation defers, one row per server with its tools or error
func connectPluginMCP(ctx context.Context, activation map[string]any, portable map[string]map[string]any) []any {
	deferred, _ := activation["deferred"].(map[string]any)
	var names []string
	for _, n := range toList(deferred["mcp_servers"]) {
		if s, ok := n.(string); ok {
			names = append(names, s)
		}
	}
	rows := []any{}
	if len(names) == 0 {
		return rows
	}
	mcp, err := mcpConnector()
	var configured map[string]any
	if err == nil {
		var v any
		v, err = guardedValue(func() (any, error) { return mcp.LoadConfig(ctx) })
		configured, _ = v.(map[string]any)
	}
	if err != nil {
		pkgLog().Warn("plugin MCP activation could not read server config: " + err.Error())
		for _, n := range names {
			rows = append(rows, map[string]any{"name": n, "connected": false, "tools": []any{}, "error": err.Error()})
		}
		return rows
	}
	for _, name := range names {
		config := configured[name]
		if !anyutil.IsTrue(config) {
			candidates := map[string]any{}
			if p, ok := portable[name]; ok {
				candidates[name] = p
			}
			config = mcp.FilterSuspicious(candidates)[name]
		}
		cfg, ok := config.(map[string]any)
		if !ok {
			rows = append(rows, map[string]any{"name": name, "connected": false, "tools": []any{}, "error": "no MCP configuration"})
			continue
		}
		v, err := guardedValue(func() (any, error) {
			if _, err := mcp.RegisterServers(ctx, map[string]any{name: cfg}); err != nil {
				return nil, err
			}
			registered, err := mcp.RegisteredToolNames(ctx, name)
			if err != nil {
				return nil, err
			}
			suffixes := utilitySuffixes(mcp)
			kept := []string{}
			for _, t := range registered {
				if !hasAnySuffix(t, suffixes) {
					kept = append(kept, t)
				}
			}
			return kept, nil
		})
		if err != nil {
			rows = append(rows, map[string]any{"name": name, "connected": false, "tools": []any{}, "error": err.Error()})
			continue
		}
		tools, _ := v.([]string)
		row := map[string]any{"name": name, "connected": len(tools) > 0, "tools": anyList(tools)}
		if len(tools) == 0 {
			if reason := serverError(ctx, mcp, name); anyutil.IsTrue(reason) {
				row["error"] = reason
			} else {
				row["error"] = "the server did not connect"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// Reports whether the name ends with any of the suffixes
func hasAnySuffix(name string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Resource and prompt wrapper suffixes every MCP server gets, left out of counts and listings
func utilitySuffixes(mcp MCPConnector) []string {
	kinds := mcp.UtilityKinds()
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = "_" + k
	}
	return out
}

// Error the MCP status reports for a server, nil when none or the status read fails
func serverError(ctx context.Context, mcp MCPConnector, name string) any {
	v, err := guardedValue(func() (any, error) { return mcp.Status(ctx) })
	if err != nil {
		return nil
	}
	entries, _ := v.([]map[string]any)
	for _, entry := range entries {
		if n, ok := entry["name"].(string); ok && n == name {
			return entry["error"]
		}
	}
	return nil
}

// Name and description of every skill the plugin key registered, in qualified name order
func pluginSkills(ctx context.Context, pluginKey string) []any {
	m := GetPluginManager(ctx)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []any{}
	for _, qualified := range sortedKeys(m.pluginSkills) {
		if entry := m.pluginSkills[qualified]; entry.PluginKey == pluginKey {
			out = append(out, map[string]any{"name": qualified, "description": entry.Description})
		}
	}
	return out
}

// Note an open chat receives naming the live skills and MCP servers, empty when nothing is live
func liveNotice(activation map[string]any) (string, error) {
	live, _ := activation["live_now"].(map[string]any)
	servers, skills := toList(live["mcp_servers"]), toList(live["skills"])
	if len(servers) == 0 && len(skills) == 0 {
		return "", nil
	}
	name := activation["name"]
	if !anyutil.IsTrue(name) {
		name = activation["key"]
	}
	lines := []string{"[System note: plugin " + anyutil.String(name) + " was installed and is live in this chat now (an Icebath note, " +
		"not tool output)."}
	for _, raw := range skills {
		skill, _ := raw.(map[string]any)
		lines = append(lines, "- Skill "+anyutil.String(skill["name"])+": load it with skill_view.")
	}
	anyConnected := false
	for _, raw := range servers {
		server, _ := raw.(map[string]any)
		serverName := anyutil.String(server["name"])
		if anyutil.IsTrue(server["connected"]) {
			anyConnected = true
			prefix, err := toolPrefix(serverName)
			if err != nil {
				return "", err
			}
			n, err := lenOr(server["tools"])
			if err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("- MCP server %s: %d tools, %s*.", serverName, n, prefix))
		} else {
			reason := "unknown error"
			if e := server["error"]; anyutil.IsTrue(e) {
				reason = anyutil.String(e)
			}
			lines = append(lines, "- MCP server "+serverName+": not connected ("+reason+").")
		}
	}
	if anyConnected {
		lines = append(lines, "Its MCP tools are deferred: find them with tool_search, read one with tool_describe, "+
			"run it with tool_call.")
	}
	deferred, _ := activation["deferred"].(map[string]any)
	var later []string
	if anyutil.IsTrue(deferred["tools"]) {
		n, err := lenOr(deferred["tools"])
		if err != nil {
			return "", err
		}
		later = append(later, fmt.Sprintf("%d Python tools", n))
	}
	if anyutil.IsTrue(deferred["prompt"]) {
		n, err := lenOr(deferred["prompt"])
		if err != nil {
			return "", err
		}
		later = append(later, fmt.Sprintf("%d prompt sections", n))
	}
	if len(later) > 0 {
		lines = append(lines, "Next session: "+strings.Join(later, ", ")+".")
	}
	return strings.Join(lines, "\n") + "]", nil
}

// Start of every registered tool name of a server
func toolPrefix(server string) (string, error) {
	mcp, err := mcpConnector()
	if err != nil {
		return "", err
	}
	return mcp.ToolNamePrefix() + mcp.SanitizeNameComponent(server) + "__", nil
}
