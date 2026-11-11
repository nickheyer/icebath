package plugins

import "context"

// Context bound to the home of the manager loading this plugin, what load_config resolves during register
func (c *Context) HomeContext() context.Context {
	if c.manager == nil {
		return context.Background()
	}
	return pluginHomeScope(context.Background(), c.manager.home)
}
