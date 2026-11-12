package app

// Plugin command names of the gateway catalog for completion, nil off the gateway so the plugin registry answers
func pluginCommandNames(c *CLI) map[string]string {
	found, onGateway := gatewayPluginNames(c)
	if !onGateway {
		return nil
	}
	return found
}
