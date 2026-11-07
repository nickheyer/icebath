package plugins

import (
	"context"
	"path/filepath"
	"strings"
)

const (
	computerUseProviderBase    = "tools.computer_use.backend:ComputerUseProvider"
	computerUseProviderCapture = "register_computer_use_provider"
)

// Loads a user computer use provider dir in the plugin host, nil when it registers no provider
func LoadComputerUseProviderInHost(ctx context.Context, providerDir, moduleName string) (*HostedObject, error) {
	host, err := GetPluginManager(ctx).PluginHost()
	if err != nil {
		return nil, err
	}
	name := filepath.Base(providerDir)
	var namespace any
	if i := strings.LastIndex(moduleName, "."); i >= 0 {
		namespace = moduleName[:i]
	}
	load := func(ctx context.Context) (map[string]any, error) {
		return host.instanceRef(ctx, computerUseProviderCapture+":"+name, "load_computer_use_provider", map[string]any{
			"name": name, "path": providerDir, "module_name": moduleName, "synthetic_namespace": namespace,
			"ctx_methods": []any{},
		}, nil)
	}
	return host.reloadableInstance(ctx, computerUseProviderCapture, name, computerUseProviderBase, load, nil)
}
