package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Provider names of the active scope home that category config switches on
func CategoryActiveNames(ctx context.Context) map[string]bool {
	return categoryActiveNames(home.Dir(ctx))
}

// Activation state of a listed plugin as plugins list prints it, its directory deciding default on kinds
func ListedPluginStatus(ctx context.Context, e PluginEntry, enabled, disabled, active map[string]bool) (string, error) {
	return pluginStatus(home.Dir(ctx), e, enabled, disabled, active, true)
}

// Configured memory provider of the active scope home, empty when none
func CurrentMemoryProvider(ctx context.Context) string {
	return getCurrentMemoryProvider(home.Dir(ctx))
}

// Configured context engine of the active scope home, compressor when none
func CurrentContextEngine(ctx context.Context) string { return getCurrentContextEngine(home.Dir(ctx)) }

// Kill list reason of an installed plugin of the active scope home, false when it is not removed
func RemovedAnnotation(ctx context.Context, name, dir string, removedEntries []RemovedEntry) (string, bool, error) {
	return removedAnnotation(home.Dir(ctx), name, dir, removedEntries)
}

// Manifest of a listed plugin, a compiled entry read from its registration or the compiled tree, empty when unreadable
func ListedPluginManifest(e PluginEntry) map[string]any {
	if e.Dir == "" {
		for _, m := range bundledManifests() {
			if m.Key == e.Key {
				provided := make([]any, len(m.ProvidesTools))
				for i, name := range m.ProvidesTools {
					provided[i] = name
				}
				return map[string]any{"name": m.Name, "version": m.Version, "description": m.Description, "kind": m.Kind, "provides_tools": provided}
			}
		}
		if manifest, ok := compiledBundledManifest(e.Key); ok {
			return manifest
		}
		return map[string]any{}
	}
	manifest, err := readManifest(e.Dir)
	if err != nil || manifest == nil {
		return map[string]any{}
	}
	return manifest
}
