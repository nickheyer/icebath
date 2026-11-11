package plugins

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyimport"
)

// Entry point group whose names declare plugin capabilities as plugin-id.capability-id
const entryPointCapabilitiesGroup = "icebath_agent.plugin_capabilities"

// Metadata only manifests of installed entry point plugins, kind from an import free source scan
func discoverEntrypointManifests(ctx context.Context) []*Manifest {
	path := processImportPath()
	groupEPs := entryPointGroup(path, pipEntryPointGroup)
	capabilityEPs := entryPointGroup(path, entryPointCapabilitiesGroup)
	manifests := []*Manifest{}
	for _, ep := range groupEPs {
		declared := map[string]bool{}
		for _, d := range capabilityEPs {
			if d.Value == ep.Value {
				declared[d.Name] = true
			}
		}
		var capabilities []any
		for _, c := range ValidCapabilityIDs() {
			if declared[ep.Name+"."+c] {
				capabilities = append(capabilities, c)
			}
		}
		manifests = append(manifests, &Manifest{
			Name: ep.Name, Version: distributionHeader(ep.Dist, "Version"),
			Description: distributionHeader(ep.Dist, "Summary"),
			Source:      "entrypoint", Path: ep.Value, Key: ep.Name,
			Kind:         classifyEntrypointValueKind(path, ep.Value),
			Capabilities: ParseDeclaredCapabilities(capabilities, ep.Name), ManifestVersion: 1,
		})
	}
	return manifests
}

// Kind of an entry point target from its module source, standalone when unresolvable
func classifyEntrypointValueKind(path []string, value string) string {
	module, _, _ := strings.Cut(value, ":")
	module = strings.TrimSpace(module)
	if module == "" {
		return "standalone"
	}
	if kind := detectKindFromSource(resolveModuleSource(path, module, 8192)); kind != "" {
		return kind
	}
	return "standalone"
}

// First limit characters of a module source found on path without importing it, empty when unresolvable
func resolveModuleSource(path []string, module string, limit int) string {
	return pyimport.ReadSource(pyimport.FindOrigin(path, module), limit)
}

// Manifests of the installed plugin entry points, read without importing plugin code
func (m *Manager) scanEntryPoints(ctx context.Context) []*Manifest {
	return discoverEntrypointManifests(ctx)
}
