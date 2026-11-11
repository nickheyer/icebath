package plugins

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Synthetic package user installed memory provider packages load under
const UserMemoryNamespace = "_icebath_user_memory"

// Parent packages a memory provider package is imported beneath
var providerModuleParents = []any{"plugins", "plugins.memory"}

// Object base of an imported provider module whose classes, submodules and instances stay in the host as proxies
const hostedModuleBase = "provider_module"

// Whether a provider package directory sits directly in the bundled memory plugins directory
func bundledMemoryProviderDir(pluginDir string) bool {
	dir, err := pypath.Realpath(pluginDir, pypath.Lax)
	if err != nil {
		dir = pluginDir
	}
	override := bundledOverrideDir()
	if override == "" {
		return false
	}
	root := filepath.Join(override, "memory")
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
	params, err := providerModuleParams(pluginDir, moduleName, submodule)
	if err != nil {
		return nil, false, err
	}
	params["function"], params["args"], params["kwargs"] = function, h.encodeArgs(nonNilArgs(args)), h.encodeArgs(nonNilKwargs(kwargs))
	raw, err := h.call(ctx, "provider_module_call", params, nil, 0, nil)
	if err != nil {
		return nil, true, err
	}
	reply, _ := raw.(map[string]any)
	if loaded, _ := reply["loaded"].(bool); !loaded {
		return nil, false, nil
	}
	return reply["result"], true, nil
}

// Load parameters of a provider package under its loader module name, user packages beneath the synthetic namespace
func providerModuleParams(pluginDir, moduleName, submodule string) (map[string]any, error) {
	var namespace any
	if !bundledMemoryProviderDir(pluginDir) {
		namespace = UserMemoryNamespace
	}
	root, err := hostPluginsRoot()
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": pluginDir, "module_name": moduleName, "parents": providerModuleParents,
		"plugins_root": root, "synthetic_namespace": namespace, "submodule": submodule}, nil
}

// Imports a provider package or one submodule in the plugin host of the context home as a live module
func ImportProviderModule(ctx context.Context, name, pluginDir, moduleName, submodule string) (*HostedObject, error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, err
	}
	return host.ImportProviderModule(ctx, name, pluginDir, moduleName, submodule)
}

// Live module of a provider package or submodule in this child, an ImportError when the package fails to import
func (h *PluginHost) ImportProviderModule(ctx context.Context, name, pluginDir, moduleName, submodule string) (*HostedObject, error) {
	params, err := providerModuleParams(pluginDir, moduleName, submodule)
	if err != nil {
		return nil, err
	}
	load := func(ctx context.Context) (map[string]any, error) {
		if _, err := h.ensureStarted(ctx); err != nil {
			return nil, err
		}
		raw, err := h.callIn(ctx, hostedRawRefBase, "provider_module_import", params, nil, 0, nil)
		if err != nil {
			return nil, err
		}
		reply, _ := raw.(map[string]any)
		ref, _ := reply["module"].(map[string]any)
		if loaded, _ := reply["loaded"].(bool); !loaded || ref == nil {
			return nil, &HostBuiltinError{Type: "ImportError", Message: fmt.Sprintf("memory provider %s failed to import", config.PyRepr(name))}
		}
		return h.stampGeneration(ref), nil
	}
	ref, err := load(ctx)
	if err != nil {
		return nil, err
	}
	return h.objectProxy(ref, hostedModuleBase, load), nil
}

// Plugin host of every manager this process created whose child is running, the interpreters one hermes process maps to
func RunningPluginHosts() []*PluginHost {
	var hosts []*PluginHost
	for _, m := range knownPluginManagers() {
		pluginHostMu.Lock()
		host := m.pluginHostInstance
		pluginHostMu.Unlock()
		if host != nil && host.Alive() {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
