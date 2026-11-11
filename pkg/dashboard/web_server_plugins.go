package dashboard

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

var (
	dashboardPluginsMu    sync.Mutex
	dashboardPluginsCache []*pyjson.Object
)

// Dashboard plugins of the active scope, a secondary profile's own and the launch profile's cached
func getDashboardPlugins(ctx context.Context, forceRescan bool) []*pyjson.Object {
	if h, ok := secondaryProfileHome(ctx); ok {
		return profileDashboardPlugins(h, forceRescan)
	}
	dashboardPluginsMu.Lock()
	defer dashboardPluginsMu.Unlock()
	if dashboardPluginsCache == nil || forceRescan || pluginDirsMissing(dashboardPluginsCache) {
		dashboardPluginsCache = discoverDashboardPlugins(nil)
	}
	return dashboardPluginsCache
}
