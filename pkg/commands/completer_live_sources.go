package commands

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Skin names with their description, else their source, the /skin completion rows
func LiveSkinRows(ctx context.Context) []NameMeta {
	var rows []NameMeta
	for _, s := range config.ListSkins(ctx) {
		meta := pyrepr.Str(s.Description)
		if !pyrepr.Truthy(s.Description) {
			meta = s.Source
		}
		rows = append(rows, NameMeta{pyrepr.Str(s.Name), meta})
	}
	return rows
}

// Configurable and plugin toolsets with their cli enable state from the readonly config of the ctx profile
func LiveToolsetRows(ctx context.Context) []ToolsetRow {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return nil
	}
	enabled := tools.Default.PlatformTools(ctx, "", cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: false})
	var rows []ToolsetRow
	for _, ts := range tools.ConfigurableToolsets {
		rows = append(rows, ToolsetRow{Key: ts.Key, Label: ts.Label, Enabled: enabled[ts.Key]})
	}
	pluginKeys := tools.PluginToolsetKeys(ctx)
	keys := make([]string, 0, len(pluginKeys))
	for k := range pluginKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rows = append(rows, ToolsetRow{Key: k, Label: "plugin toolset", Enabled: enabled[k]})
	}
	return rows
}

// Connected gateway platforms with their home channel name, the /handoff completion rows
func LiveConnectedPlatforms(ctx context.Context) []PlatformHome {
	gw, err := gateway.LoadGatewayConfig(ctx)
	if err != nil || gw == nil {
		return nil
	}
	var out []PlatformHome
	for _, p := range gw.GetConnectedPlatforms(ctx) {
		row := PlatformHome{Name: string(p)}
		if h := gw.GetHomeChannel(p); h != nil && pyrepr.Truthy(h.Name) {
			row.Home = pyrepr.Str(h.Name)
		}
		out = append(out, row)
	}
	return out
}
