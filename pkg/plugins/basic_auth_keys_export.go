package plugins

// Whether a plugins.disabled entry names the bundled basic dashboard auth plugin
func IsBasicAuthPluginKey(name string) bool { return basicAuthPluginKeys[name] }
