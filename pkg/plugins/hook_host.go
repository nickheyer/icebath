package plugins

import "context"

// Imports a gateway hook handler.py in the plugin host of the home in ctx, returning its handle attribute or why it was skipped
func LoadGatewayHook(ctx context.Context, hookName, handlerPath string) (any, string, error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, "", err
	}
	if _, err := host.ensureStarted(ctx); err != nil {
		return nil, "", err
	}
	raw, err := host.call(ctx, "load_hook", map[string]any{"hook_name": hookName, "path": handlerPath}, nil, 0, nil)
	if err != nil {
		return nil, "", err
	}
	reply, _ := raw.(map[string]any)
	if skip, ok := reply["skip"].(string); ok {
		return nil, skip, nil
	}
	return reply["handle"], "", nil
}
