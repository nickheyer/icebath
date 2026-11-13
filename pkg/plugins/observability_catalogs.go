package plugins

import (
	"context"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/observability"
)

func init() {
	observability.SetCatalogSource("plugin_catalog_names", pluginCatalogStems)
	observability.SetCatalogSource("catalog_platform_names", catalogPlatformNames)
	observability.BundledPluginKeysHook = BundledPluginKeys
	observability.PluginCommandHandlerHook = func(ctx context.Context, name string) bool {
		handler, ok := GetPluginCommandHandler(ctx, name)
		return ok && handler != nil
	}
}

// Lower case file stems of the in-tree plugin catalog
func pluginCatalogStems() ([]string, error) {
	root, err := fs.Sub(inTreeCatalog, "catalog")
	if err != nil {
		return nil, err
	}
	return observability.YamlStems(root), nil
}

// In-tree catalog entries of a known tier whose category is platform, never a network fetch
func catalogPlatformNames() ([]string, error) {
	var names []string
	for _, e := range LoadCatalog("") {
		if e.Category == "platform" && containsString(CatalogTiers, e.Tier) {
			names = append(names, e.Name)
		}
	}
	return names, nil
}
