package mcp

import (
	"context"

	"github.com/nickheyer/icebath/pkg/plugins"
)

// Config server entries plus the safe plugin portable ones, each name mapped to its owning plugin or nil
func ServerConfigsWithSources(ctx context.Context, configServers map[string]any) (map[string]map[string]any, map[string]*string) {
	servers := map[string]map[string]any{}
	owned := map[string]*string{}
	for name, cfg := range configServers {
		m, ok := cfg.(map[string]any)
		if !ok {
			continue
		}
		copied := make(map[string]any, len(m))
		for k, v := range m {
			copied[k] = v
		}
		servers[name] = copied
		owned[name] = nil
	}
	plugins.DiscoverPlugins(ctx, false)
	manager := plugins.GetPluginManager(ctx)
	raw := map[string]any{}
	for name, cfg := range manager.GetPortableMCPServers() {
		raw[name] = cfg
	}
	portable := filterSuspiciousServers(raw)
	owners := manager.GetPortableMCPServerPlugins()
	for name, cfg := range portable {
		if _, seen := servers[name]; seen {
			continue
		}
		m, _ := cfg.(map[string]any)
		copied := make(map[string]any, len(m))
		for k, v := range m {
			copied[k] = v
		}
		servers[name] = copied
		if owner, ok := owners[name]; ok {
			o := owner
			owned[name] = &o
		} else {
			owned[name] = nil
		}
	}
	return servers, owned
}
