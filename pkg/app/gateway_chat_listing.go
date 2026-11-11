package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Enabled and disabled toolsets of the listing, an explicit toolsets flag or the profile CLI selection
func gatewayListingSelection(ctx context.Context, args map[string]any) ([]string, []string, error) {
	requested := args["toolsets"]
	if list, ok := requested.([]string); ok {
		requested = strings.Join(list, ",")
	} else if list, ok := requested.([]any); ok {
		parts := make([]string, len(list))
		for i, item := range list {
			parts[i] = pyrepr.Str(item)
		}
		requested = strings.Join(parts, ",")
	}
	if text, ok := requested.(string); ok && pystr.Strip(text) != "" {
		seen := map[string]bool{}
		names := []string{}
		for _, name := range strings.Split(text, ",") {
			if name = pystr.Strip(name); name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		sort.Strings(names)
		return names, []string{}, nil
	}
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, nil, err
	}
	disabled := []string{}
	if raw, ok := cfg.Sub("agent")["disabled_toolsets"]; ok && pyrepr.Truthy(raw) {
		switch list := raw.(type) {
		case []any:
			for _, item := range list {
				disabled = append(disabled, pyrepr.Str(item))
			}
		case []string:
			disabled = append(disabled, list...)
		}
	}
	enabled := sortedKeys(tools.Default.PlatformTools(ctx, tools.ScopeKey(h), cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true}))
	return enabled, disabled, nil
}

// Prints the tool or toolset catalog of this profile CLI selection with no session and no gateway
func printToolListing(ctx context.Context, args map[string]any) (int, error) {
	out := console().Out
	t := func(key string, kw map[string]any) string { return i18n.T(ctx, key, "", kw) }
	enabled, disabled, err := gatewayListingSelection(ctx, args)
	if err != nil {
		return 0, err
	}
	env := bannerToolEnv(ctx)
	if pyrepr.Truthy(args["list_tools"]) {
		defs := tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: enabled, Disabled: disabled,
			Quiet: true, SkipToolSearchAssembly: true})
		if len(defs) == 0 {
			fmt.Fprintln(out, t("cli.tools.none_available", nil))
			return 0, nil
		}
		toolName := func(def map[string]any) string {
			fn, _ := def["function"].(map[string]any)
			return pyrepr.Str(requireKey(fn, "name"))
		}
		sorted := append([]map[string]any(nil), defs...)
		sort.SliceStable(sorted, func(i, j int) bool { return toolName(sorted[i]) < toolName(sorted[j]) })
		type row struct{ name, desc string }
		grouped := map[string][]row{}
		for _, def := range sorted {
			name := toolName(def)
			fn, _ := def["function"].(map[string]any)
			desc := ""
			if raw, ok := fn["description"]; ok {
				desc = pyrepr.Str(raw)
			}
			desc, _, _ = strings.Cut(desc, "\n")
			if i := strings.Index(desc, ". "); i >= 0 {
				desc = desc[:i+1]
			}
			toolset := tools.Default.ToolsetForTool(ctx, env.Home, name)
			if toolset == "" {
				toolset = "unknown"
			}
			grouped[toolset] = append(grouped[toolset], row{name, desc})
		}
		fmt.Fprintln(out, t("cli.tools.header", nil))
		for _, toolset := range sortedMapKeys(grouped) {
			fmt.Fprintf(out, "\n  [%s]\n", toolset)
			for _, r := range grouped[toolset] {
				fmt.Fprintln(out, pyFmt("    * {name:<20} - {desc}", map[string]any{"name": r.name, "desc": r.desc}))
			}
		}
		fmt.Fprintf(out, "\n  %s\n", t("cli.tools.total", map[string]any{"count": strconv.Itoa(len(defs))}))
		return 0, nil
	}
	fmt.Fprintln(out, t("cli.toolsets.header", nil))
	names := []string{}
	for name := range tools.Default.AllToolsets(ctx, "") {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		info, ok := tools.Default.ToolsetInfoFor(ctx, "", name)
		if !ok {
			continue
		}
		marker := "   "
		for _, e := range enabled {
			if e == name {
				marker = "(*)"
				break
			}
		}
		fmt.Fprintln(out, pyFmt("  {marker} {name:<18} [{count:>2} tools] - {desc}",
			map[string]any{"marker": marker, "name": name, "count": info.ToolCount, "desc": info.Description}))
	}
	fmt.Fprintf(out, "\n  %s\n", t("cli.toolsets.currently_enabled_legend", nil))
	return 0, nil
}
