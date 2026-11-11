package gatewayrun

import "github.com/nickheyer/icebath/pkg/plugins"

// Removes this runner's plugin message scheduler without clobbering a newer owner
func (r *GatewayRunner) clearPluginMessageInjector() {
	plugins.ClearPublishedGatewayMessageHost(r)
}
