package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Checklist indexes enabled by an include whitelist, an exclude list, or everything without a filter
func mcpPreselected(toolNames []string, include, exclude map[string]bool) map[int]bool {
	pre := map[int]bool{}
	for i, tn := range toolNames {
		switch {
		case include != nil:
			if MatchesNameFilter(tn, include) {
				pre[i] = true
			}
		case len(exclude) > 0:
			if !MatchesNameFilter(tn, exclude) {
				pre[i] = true
			}
		default:
			pre[i] = true
		}
	}
	return pre
}

// Writes a checklist result into a tools block, keeping exclude mode globs and literal toggles
func applyMCPChecklist(ui TerminalUI, serverName string, toolsCfg map[string]any, toolNames []string, chosen map[int]bool, include, exclude map[string]bool) {
	excludeMode := len(exclude) > 0 && include == nil
	count := len(sortedIndexes(chosen))
	switch {
	case count == len(toolNames) && !excludeMode:
		delete(toolsCfg, "exclude")
		delete(toolsCfg, "include")
	case excludeMode:
		old := make([]string, 0, len(exclude))
		for p := range exclude {
			old = append(old, p)
		}
		sort.Strings(old)
		var globs []string
		literals := map[string]bool{}
		for _, p := range old {
			if strings.ContainsAny(p, "*?[") {
				globs = append(globs, p)
			} else {
				literals[p] = true
			}
		}
		globSet := map[string]bool{}
		for _, g := range globs {
			globSet[g] = true
		}
		shadowSet := map[string]bool{}
		for i, tn := range toolNames {
			if chosen[i] {
				delete(literals, tn)
				if len(globs) > 0 && MatchesNameFilter(tn, globSet) {
					shadowSet[tn] = true
				}
			}
		}
		for i, tn := range toolNames {
			if !chosen[i] && !MatchesNameFilter(tn, exclude) {
				literals[tn] = true
			}
		}
		if len(shadowSet) > 0 {
			printWarning(ui, fmt.Sprintf("  %s: %d re-enabled tool(s) still match glob exclude pattern(s) %s and stay excluded — edit mcp_servers.%s.tools.exclude in config.yaml to enable them.",
				serverName, len(shadowSet), pyStrListRepr(globs), serverName))
		}
		sortedLiterals := make([]string, 0, len(literals))
		for s := range literals {
			sortedLiterals = append(sortedLiterals, s)
		}
		sort.Strings(sortedLiterals)
		if newExclude := append(toAnyList(globs), toAnyList(sortedLiterals)...); len(newExclude) > 0 {
			toolsCfg["exclude"] = newExclude
		} else {
			delete(toolsCfg, "exclude")
		}
		delete(toolsCfg, "include")
	default:
		var names []any
		for _, i := range sortedIndexes(chosen) {
			names = append(names, toolNames[i])
		}
		toolsCfg["include"] = names
		delete(toolsCfg, "exclude")
	}
}

// Probe seam of the tools configurator, the real all server probe outside tests
var toolsConfigProbe = Probe

// Probes every enabled server and writes per server checklist choices back as tools include or exclude
func configureMCPToolsInteractive(ctx context.Context, env *tools.Env, ui TerminalUI, cfg config.Tree) error {
	servers, _ := cfg["mcp_servers"].(map[string]any)
	if len(servers) == 0 {
		printInfo(ui, "No MCP servers configured.")
		return nil
	}
	var enabled []string
	for _, name := range sortedKeys(servers) {
		if entry, ok := servers[name].(map[string]any); ok && ServerEnabled(entry) {
			enabled = append(enabled, name)
		}
	}
	if len(enabled) == 0 {
		printInfo(ui, "All MCP servers are disabled.")
		return nil
	}
	out := ui.Out()
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Color("  Discovering tools from MCP servers...", ansiYellow))
	fmt.Fprintln(out, ui.Color(fmt.Sprintf("  Connecting to %d server(s): %s", len(enabled), strings.Join(enabled, ", ")), ansiDim))
	serverTools := toolsConfigProbe(ctx, env)
	if len(serverTools) == 0 {
		printWarning(ui, "Could not discover tools from any MCP server.")
		printInfo(ui, "Check that server commands/URLs are correct and dependencies are installed.")
		return nil
	}
	total := 0
	for _, name := range enabled {
		if _, ok := serverTools[name]; !ok {
			printWarning(ui, fmt.Sprintf("  Could not connect to '%s'", name))
		}
	}
	probed := make([]string, 0, len(serverTools))
	for name, found := range serverTools {
		probed = append(probed, name)
		total += len(found)
	}
	sort.Strings(probed)
	fmt.Fprintln(out, ui.Color(fmt.Sprintf("  Found %d tool(s) across %d server(s)", total, len(serverTools)), ansiGreen))
	fmt.Fprintln(out)
	anyChanges := false
	for _, name := range probed {
		found := serverTools[name]
		if len(found) == 0 {
			printInfo(ui, fmt.Sprintf("  %s: no tools found", name))
			continue
		}
		entry, _ := servers[name].(map[string]any)
		existing, _ := entry["tools"].(map[string]any)
		var include, exclude map[string]bool
		if list, ok := existing["include"].([]any); ok {
			include = map[string]bool{}
			for _, p := range list {
				include[pyStr(p)] = true
			}
		}
		if list, _ := existing["exclude"].([]any); len(list) > 0 {
			exclude = map[string]bool{}
			for _, p := range list {
				exclude[pyStr(p)] = true
			}
		}
		labels := make([]string, len(found))
		toolNames := make([]string, len(found))
		for i, t := range found {
			desc := t.Description
			if r := []rune(desc); len(r) > 70 {
				desc = string(r[:70]) + "..."
			}
			labels[i] = t.Name
			if desc != "" {
				labels[i] = t.Name + "  (" + desc + ")"
			}
			toolNames[i] = t.Name
		}
		pre := mcpPreselected(toolNames, include, exclude)
		chosen := ui.Checklist(fmt.Sprintf("MCP Server: %s  (%d tools)", name, len(found)), labels, pre, pre)
		if sameIndexSet(chosen, pre) {
			printInfo(ui, fmt.Sprintf("  %s: no changes", name))
			continue
		}
		if entry == nil {
			entry = map[string]any{}
			servers[name] = entry
		}
		toolsCfg, ok := entry["tools"].(map[string]any)
		if !ok {
			toolsCfg = map[string]any{}
			entry["tools"] = toolsCfg
		}
		applyMCPChecklist(ui, name, toolsCfg, toolNames, chosen, include, exclude)
		n := len(sortedIndexes(chosen))
		printSuccess(ui, fmt.Sprintf("  %s: %d enabled, %d disabled", name, n, len(found)-n))
		anyChanges = true
	}
	if !anyChanges {
		fmt.Fprintln(out, ui.Color("  No changes to MCP tools", ansiDim))
		return nil
	}
	if err := config.Save(home.Dir(ctx), cfg); err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, ui.Color("  ✓ MCP tool configuration saved", ansiGreen))
	return nil
}

// Adds or removes toolset keys from a platform's saved toolsets
func applyToolsetChange(ctx context.Context, cfg config.Tree, platform string, names []string, action string) error {
	pluginKeys := tools.PluginToolsetKeys(ctx)
	enabled := tools.Default.PlatformTools(ctx, "", cfg, platform, tools.PlatformToolsOptions{PluginToolsetKeys: pluginKeys})
	for _, n := range names {
		if action == "disable" {
			delete(enabled, n)
		} else {
			enabled[n] = true
		}
	}
	return tools.Default.SavePlatformTools(ctx, "", cfg, platform, enabled, pluginKeys)
}

// Toggles server and tool targets in each server exclude list, returning the servers not in config
func applyMCPChange(cfg config.Tree, targets []string, action string) map[string]bool {
	failed := map[string]bool{}
	servers, _ := cfg["mcp_servers"].(map[string]any)
	for _, target := range targets {
		serverName, toolName, _ := strings.Cut(target, ":")
		entry, ok := servers[serverName].(map[string]any)
		if !ok {
			failed[serverName] = true
			continue
		}
		toolsCfg, ok := entry["tools"].(map[string]any)
		if !ok {
			toolsCfg = map[string]any{}
			entry["tools"] = toolsCfg
		}
		old, _ := toolsCfg["exclude"].([]any)
		exclude := append([]any{}, old...)
		if action != "disable" {
			kept := []any{}
			for _, t := range exclude {
				if pyStr(t) != toolName {
					kept = append(kept, t)
				}
			}
			exclude = kept
		} else {
			present := false
			for _, t := range exclude {
				present = present || pyStr(t) == toolName
			}
			if !present {
				exclude = append(exclude, toolName)
			}
		}
		toolsCfg["exclude"] = exclude
	}
	return failed
}

// Prints built in, plugin and MCP server rows with enabled state and partial strip warnings
func printToolsList(ctx context.Context, ui TerminalUI, enabled map[string]bool, servers map[string]any, platform string, stripped map[string]tools.PartialStrip) {
	out := ui.Out()
	builtin := map[string]bool{}
	for _, ts := range tools.ConfigurableToolsets {
		builtin[ts.Key] = true
	}
	var builtinRows, pluginRows []tools.ConfigurableToolset
	for _, ts := range tools.EffectiveConfigurableToolsets(ctx) {
		if !tools.ToolsetAllowedForPlatform(ts.Key, platform) {
			continue
		}
		if builtin[ts.Key] {
			builtinRows = append(builtinRows, ts)
		} else {
			pluginRows = append(pluginRows, ts)
		}
	}
	printRows := func(rows []tools.ConfigurableToolset) {
		for _, ts := range rows {
			status := ui.Color("✗ disabled", ansiRed)
			if enabled[ts.Key] {
				status = ui.Color("✓ enabled", ansiGreen)
			}
			fmt.Fprintf(out, "  %s  %s  %s\n", status, ts.Key, ui.Color(ts.Label, ansiDim))
			if ps, ok := stripped[ts.Key]; ok && enabled[ts.Key] {
				fmt.Fprintln(out, ui.Color(fmt.Sprintf("      ⚠ %s removed by agent.disabled_toolsets: %s (%s)", strings.Join(ps.Stripped, ", "), strings.Join(ps.Entries, ", "), ps.Hint), ansiYellow))
			}
		}
	}
	fmt.Fprintf(out, "Built-in toolsets (%s):\n", platform)
	printRows(builtinRows)
	if len(pluginRows) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Plugin toolsets (%s):\n", platform)
		printRows(pluginRows)
	}
	if len(servers) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "MCP servers:")
	for _, name := range sortedKeys(servers) {
		entry, _ := servers[name].(map[string]any)
		toolsCfg, _ := entry["tools"].(map[string]any)
		exclude, _ := toolsCfg["exclude"].([]any)
		if include, ok := toolsCfg["include"].([]any); ok {
			listed := "(none)"
			if len(include) > 0 {
				listed = joinAny(include)
			}
			printInfo(ui, fmt.Sprintf("%s  [include only: %s]", name, listed))
		} else if len(exclude) > 0 {
			printInfo(ui, fmt.Sprintf("%s  [excluded: %s]", name, ui.Color(joinAny(exclude), ansiYellow)))
		} else {
			printInfo(ui, fmt.Sprintf("%s  %s", name, ui.Color("all tools enabled", ansiDim)))
		}
	}
}

// Config list items joined by comma and space
func joinAny(items []any) string {
	parts := make([]string, len(items))
	for i, v := range items {
		parts[i] = pyStr(v)
	}
	return strings.Join(parts, ", ")
}

// Built in platform keys plus every platform a discovered plugin registered
func knownToolPlatforms(ctx context.Context) map[string]bool {
	known := map[string]bool{}
	for _, p := range tools.Platforms {
		known[p.Key] = true
	}
	if tools.RegisteredPlatformNames != nil {
		for _, name := range tools.RegisteredPlatformNames() {
			known[name] = true
		}
	}
	return known
}

// Lists, enables or disables toolsets and server tool MCP targets for one platform
func ToolsDisableEnableCommand(ctx context.Context, ui TerminalUI, action, platform string, targets []string) error {
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return err
	}
	builtinPlatforms := map[string]bool{}
	for _, p := range tools.Platforms {
		builtinPlatforms[p.Key] = true
	}
	valid := builtinPlatforms
	if !valid[platform] {
		valid = knownToolPlatforms(ctx)
	}
	if !valid[platform] {
		names := make([]string, 0, len(valid))
		for k := range valid {
			names = append(names, k)
		}
		sort.Strings(names)
		printError(ui, fmt.Sprintf("Unknown platform '%s'. Valid: %s", platform, strings.Join(names, ", ")))
		return nil
	}
	pluginKeys := tools.PluginToolsetKeys(ctx)
	if action == "list" {
		enabled := tools.Default.PlatformTools(ctx, "", cfg, platform, tools.PlatformToolsOptions{PluginToolsetKeys: pluginKeys})
		servers, _ := cfg["mcp_servers"].(map[string]any)
		printToolsList(ctx, ui, enabled, servers, platform, tools.Default.ToolsetsPartiallyStripped(ctx, "", cfg, enabled))
		return nil
	}
	var toolsetTargets, mcpTargets []string
	for _, t := range targets {
		if strings.Contains(t, ":") {
			mcpTargets = append(mcpTargets, t)
		} else {
			toolsetTargets = append(toolsetTargets, t)
		}
	}
	validToolsets := map[string]bool{}
	for _, ts := range tools.ConfigurableToolsets {
		validToolsets[ts.Key] = true
	}
	for k := range pluginKeys {
		validToolsets[k] = true
	}
	rejected := map[string]bool{}
	for _, name := range toolsetTargets {
		if !validToolsets[name] {
			printError(ui, fmt.Sprintf("Unknown toolset '%s'", name))
			rejected[name] = true
		}
	}
	for _, name := range toolsetTargets {
		if validToolsets[name] && !tools.ToolsetAllowedForPlatform(name, platform) {
			var allowed []string
			for p := range tools.ToolsetPlatformRestrictions[name] {
				allowed = append(allowed, p)
			}
			sort.Strings(allowed)
			printError(ui, fmt.Sprintf("Toolset '%s' is not available on platform '%s' (only: %s)", name, platform, strings.Join(allowed, ", ")))
			rejected[name] = true
		}
	}
	var accepted []string
	for _, t := range toolsetTargets {
		if !rejected[t] {
			accepted = append(accepted, t)
		}
	}
	if len(accepted) > 0 {
		if err := applyToolsetChange(ctx, cfg, platform, accepted, action); err != nil {
			return err
		}
	}
	failed := map[string]bool{}
	if len(mcpTargets) > 0 {
		failed = applyMCPChange(cfg, mcpTargets, action)
		for _, srv := range sortedKeys(boolMapAny(failed)) {
			printError(ui, fmt.Sprintf("MCP server '%s' not found in config", srv))
		}
	}
	if err := config.Save(h, cfg); err != nil {
		return err
	}
	var successful []string
	for _, t := range targets {
		server, _, isMCP := strings.Cut(t, ":")
		if !rejected[t] && (!isMCP || !failed[server]) {
			successful = append(successful, t)
		}
	}
	if len(successful) > 0 {
		verb := "Enabled"
		if action == "disable" {
			verb = "Disabled"
		}
		printSuccess(ui, verb+": "+strings.Join(successful, ", "))
	}
	return nil
}

// Bool set as a generic map for sorted key listing
func boolMapAny(m map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
