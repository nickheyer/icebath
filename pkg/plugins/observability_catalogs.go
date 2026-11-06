package plugins

import (
	"io/fs"
	"path"
	"strings"

	"github.com/nickheyer/icebath/pkg/observability"
)

func init() {
	observability.SetCatalogSource("plugin_catalog_names", pluginCatalogStems)
	observability.SetCatalogSource("catalog_platform_names", catalogPlatformNames)
}

// Lower case file stems of the in-tree plugin catalog
func pluginCatalogStems() ([]string, error) {
	files, err := fs.Glob(inTreeCatalog, "catalog/*.yaml")
	if err != nil {
		return nil, err
	}
	stems := make([]string, 0, len(files))
	for _, f := range files {
		stems = append(stems, strings.ToLower(strings.TrimSuffix(path.Base(f), ".yaml")))
	}
	return stems, nil
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
