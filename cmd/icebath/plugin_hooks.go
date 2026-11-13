package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Plugin manager entry points provider resolution and the left core migration reach through the plugins, memory and pm packages
func pluginManagerHooks() provider.PluginManagerHooks {
	return provider.PluginManagerHooks{
		CatalogEntry: memory.CatalogSource,
		Install: func(ctx context.Context, name string, _ bool) (map[string]any, error) {
			return plugins.DashboardInstallPlugin(ctx, plugins.DashboardInstallOptions{Enable: true, CatalogName: name})
		},
		LoadError:       func(plugin string) string { return provider.PluginLoadError(context.Background(), plugin) },
		DependencyHomes: func() ([]string, error) { return pm.DependencyHomes(context.Background()) },
	}
}
