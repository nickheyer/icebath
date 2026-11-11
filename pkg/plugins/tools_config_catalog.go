package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

func init() {
	tools.PluginToolsetCatalog.Discover = func(ctx context.Context) { DiscoverPlugins(ctx, false) }
	tools.PluginToolsetCatalog.Toolsets = func(ctx context.Context) []tools.ConfigurableToolset {
		rows := GetPluginToolsets(ctx)
		out := make([]tools.ConfigurableToolset, 0, len(rows))
		for _, ts := range rows {
			out = append(out, tools.ConfigurableToolset{Key: ts.Key, Label: ts.Label, Description: ts.Description})
		}
		return out
	}
	tools.PluginToolsetCatalog.KeysNowait = GetPluginToolsetKeysNowait
	tools.PluginToolsetCatalog.PortableNowait = GetPortableMCPServerNamesNowait
}
