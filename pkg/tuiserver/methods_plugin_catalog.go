package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Seams over the live catalog and its kill list, swapped only where a hermes case patches them
var (
	catalogLoadLive       = plugins.LoadCatalogLive
	catalogRemovedEntries = plugins.ResolvedRemovedEntries
	pluginCatalogRegistry = &handlerRegistry{}
)

func init() {
	pluginCatalogRegistry.scopedMethod("plugins.catalog.lookup", pluginsCatalogLookup)
	pluginCatalogRegistry.install()
}

// One curated catalog entry by name, invalid unknown or unavailable when it cannot be offered
func pluginsCatalogLookup(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := ""
	if raw := params["name"]; pyrepr.Truthy(raw) {
		name = pystr.Strip(pyrepr.Str(raw))
	}
	if !plugins.LooksLikeCatalogName(name) {
		return ok(rid, map[string]any{"status": "invalid_name"})
	}
	entries := catalogLoadLive(ctx)
	if len(entries) == 0 {
		return ok(rid, map[string]any{"status": "unavailable"})
	}
	var entry *plugins.PluginCatalogEntry
	for _, e := range entries {
		if e.Name == name {
			entry = e
			break
		}
	}
	var removed []plugins.RemovedEntry
	if entry != nil {
		removed = catalogRemovedEntries(ctx)
	}
	if entry == nil || plugins.MatchRemoved(entry.Name, removed) != nil || plugins.MatchRemoved(entry.Repo, removed) != nil {
		return ok(rid, map[string]any{"status": "unknown"})
	}
	return ok(rid, map[string]any{"status": "found", "entry": map[string]any{"name": entry.Name, "repo": entry.Repo,
		"sha": entry.Sha, "subdir": entry.Subdir, "tier": entry.Tier}})
}
