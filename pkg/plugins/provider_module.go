package plugins

import (
	"path/filepath"

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
