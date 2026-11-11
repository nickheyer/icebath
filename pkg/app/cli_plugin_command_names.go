package app

// Nil gateway catalog because the in process CLI has no gateway connection, so completion reads the plugin registry
func pluginCommandNames(c *CLI) map[string]string {
	_ = c
	return nil
}
