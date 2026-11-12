package plugins

import "context"

// Portable install gate of a staged tree, for the external test package that wires the real availability
func RefuseUnavailablePortablePlugin(ctx context.Context, pluginName, tree, appTitle string) error {
	return refuseUnavailablePortablePlugin(ctx, pluginName, tree, appTitle)
}
