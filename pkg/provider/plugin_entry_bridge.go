package provider

import "github.com/nickheyer/icebath/pkg/plugins"

// Entry directory of an installed or bundled plugin by key, manifest name, unique leaf or catalog name
func findPluginEntryDir(plugin string) (string, bool, error) {
	return plugins.FindPluginEntryDir(plugin)
}
