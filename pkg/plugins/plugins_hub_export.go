package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Provider names of the active scope home that category config switches on
func CategoryActiveNames(ctx context.Context) map[string]bool {
	return categoryActiveNames(home.Dir(ctx))
}

// Activation state of a listed plugin as plugins list prints it, its directory deciding default on kinds
func ListedPluginStatus(ctx context.Context, e PluginEntry, enabled, disabled, active map[string]bool) (string, error) {
	return pluginStatus(home.Dir(ctx), e, enabled, disabled, active, true)
}

// Whether a plugin directory is an Agent Plugins v1 package
func IsPortablePluginDir(dir string) bool { return isPortablePluginDir(dir) }

// Full commit a ref install of the plugin is pinned to in the metadata, empty when it floats
func PinnedRevision(ctx context.Context, name string, metadata map[string]any) (string, error) {
	return pinnedRevision(home.Dir(ctx), name, metadata)
}

// Catalog sidecar of an installed plugin directory, nil when it was not installed from the catalog
func CatalogInstallRecord(ctx context.Context, pluginDir string) (map[string]any, error) {
	return catalogInstallRecord(home.Dir(ctx), pluginDir)
}

// Internal MCP server name of a portable plugin server, exactly the name its mcp.json gives it
func PortableMCPServerName(server string) string { return portableMCPServerName(server) }

// Whether a plugin key forms a plugin event name
func IsPluginEventID(key string) bool { return pyReMatch(pluginIDPattern, key) }
