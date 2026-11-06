package plugins

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Synthetic package user installed memory provider packages load under
const UserMemoryNamespace = "_icebath_user_memory"

// Parent packages a memory provider package is imported beneath
var providerModuleParents = []any{"plugins", "plugins.memory"}

// Whether a provider package directory sits directly in the bundled memory plugins directory
func bundledMemoryProviderDir(pluginDir string) bool {
	dir, err := pypath.Realpath(pluginDir, pypath.Lax)
	if err != nil {
		dir = pluginDir
	}
	root := filepath.Join(GetBundledPluginsDir(), "memory")
	if resolved, err := pypath.Realpath(root, pypath.Lax); err == nil {
		root = resolved
	}
	return filepath.Dir(dir) == root
}

// Imports a provider package submodule in the plugin host and calls one function, loaded false when the package failed to import
func CallProviderModule(ctx context.Context, pluginDir, moduleName, submodule, function string, args []any, kwargs map[string]any) (result any, loaded bool, err error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, false, err
	}
	return host.ProviderModuleCall(ctx, pluginDir, moduleName, submodule, function, args, kwargs)
}

// Runs one function of a provider package submodule in the child, loading the package under its loader name once
func (h *PluginHost) ProviderModuleCall(ctx context.Context, pluginDir, moduleName, submodule, function string, args []any, kwargs map[string]any) (any, bool, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, false, err
	}
	var namespace any
	if !bundledMemoryProviderDir(pluginDir) {
		namespace = UserMemoryNamespace
	}
	raw, err := h.call(ctx, "provider_module_call", map[string]any{"path": pluginDir, "module_name": moduleName,
		"parents": providerModuleParents, "plugins_root": GetBundledPluginsDir(), "synthetic_namespace": namespace,
		"submodule": submodule, "function": function,
		"args": wireEncode(nonNilArgs(args), nil), "kwargs": wireEncode(nonNilKwargs(kwargs), nil)}, nil, 0, nil)
	if err != nil {
		return nil, true, err
	}
	reply, _ := raw.(map[string]any)
	if loaded, _ := reply["loaded"].(bool); !loaded {
		return nil, false, nil
	}
	return reply["result"], true, nil
}
