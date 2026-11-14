package plugins

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Entry point group whose targets declare the capabilities of an entry point plugin
const pipEntryPointCapabilitiesGroup = "icebath_agent.plugin_capabilities"

// Entry points of one group in declaration order
func selectEntryPointGroup(entryPoints []pm.EntryPoint, group string) []pm.EntryPoint {
	out := []pm.EntryPoint{}
	for _, ep := range entryPoints {
		if ep.Group == group {
			out = append(out, ep)
		}
	}
	return out
}

// Metadata only manifests of the entry point plugins installed in the selected dependency environment
func DiscoverEntrypointManifests(ctx context.Context) []*Manifest {
	manifests := []*Manifest{}
	entryPoints, err := pm.SelectedEntryPoints(ctx)
	if err != nil {
		logDebug(fmt.Sprintf("Entry-point scan failed: %v", err))
		return manifests
	}
	capabilityEPs := selectEntryPointGroup(entryPoints, pipEntryPointCapabilitiesGroup)
	for _, ep := range selectEntryPointGroup(entryPoints, pipEntryPointGroup) {
		declared := map[string]bool{}
		for _, d := range capabilityEPs {
			if d.Value == ep.Value {
				declared[d.Name] = true
			}
		}
		capabilities := []any{}
		for _, id := range ValidCapabilityIDs() {
			if declared[ep.Name+"."+id] {
				capabilities = append(capabilities, id)
			}
		}
		manifests = append(manifests, &Manifest{Name: ep.Name, Version: ep.DistVersion, Description: ep.DistSummary,
			Source: "entrypoint", Path: ep.Value, Key: ep.Name, Kind: classifyEntrypointValueKind(ctx, ep.Value),
			Capabilities: ParseDeclaredCapabilities(capabilities, ep.Name), ManifestVersion: 1})
	}
	return manifests
}

// Kind of an entry point target from an import free scan of its module source, standalone when unresolvable
func classifyEntrypointValueKind(ctx context.Context, value string) string {
	moduleName, _, _ := strings.Cut(value, ":")
	moduleName = strings.TrimSpace(moduleName)
	if moduleName == "" {
		return "standalone"
	}
	if kind := detectKindFromSource(resolveModuleSource(ctx, moduleName, 8192)); kind != "" {
		return kind
	}
	return "standalone"
}

// First limit characters of the source file behind a module origin, empty when it has none
func readSourceFromOrigin(origin string, limit int) string {
	if strings.HasSuffix(origin, ".pyc") || strings.HasSuffix(origin, ".pyo") {
		origin = sourceFromCache(origin)
	}
	if origin == "" || !strings.HasSuffix(origin, ".py") {
		return ""
	}
	raw, err := os.ReadFile(origin)
	if err != nil {
		return ""
	}
	text := []rune(strings.ToValidUTF8(string(stripBOM(raw)), "�"))
	if len(text) > limit {
		text = text[:limit]
	}
	return string(text)
}

// Source path a bytecode cache file was compiled from, empty for a path outside a pycache directory
func sourceFromCache(path string) string {
	dir, file := splitLast(path)
	parent, cache := splitLast(dir)
	if cache != "__pycache__" {
		return ""
	}
	stem := strings.TrimSuffix(strings.TrimSuffix(file, ".pyc"), ".pyo")
	dots := strings.Count(stem, ".")
	if dots != 1 && dots != 2 {
		return ""
	}
	name, _, _ := strings.Cut(stem, ".")
	return parent + string(os.PathSeparator) + name + ".py"
}

// Directory and last element of a path
func splitLast(path string) (string, string) {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return "", path
	}
	return path[:i], path[i+1:]
}

// First limit characters of the source of a module found without importing it, empty when unresolvable
func resolveModuleSource(ctx context.Context, moduleName string, limit int) string {
	origin, ok := ResolveModuleOrigin(ctx, moduleName)
	if !ok {
		return ""
	}
	return readSourceFromOrigin(origin, limit)
}

// Source path of a module of the selected environment found without importing it, false when it has none
func ResolveModuleOrigin(ctx context.Context, moduleName string) (string, bool) {
	return pm.ResolveModuleOrigin(ctx, moduleName)
}

// Manifests of the installed plugin entry points for the discovery sweep
func (m *Manager) scanEntryPoints(ctx context.Context) []*Manifest {
	return DiscoverEntrypointManifests(ctx)
}
