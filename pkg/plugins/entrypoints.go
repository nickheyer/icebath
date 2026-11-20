package plugins

import (
	"context"
	"fmt"

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
		pkgLog().Debug(fmt.Sprintf("Entry-point scan failed: %v", err))
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
			Source: "entrypoint", Path: ep.Value, Key: ep.Name, Kind: "standalone",
			Capabilities: ParseDeclaredCapabilities(capabilities, ep.Name), ManifestVersion: 1})
	}
	return manifests
}

// Manifests of the installed plugin entry points for the discovery sweep
func (m *Manager) scanEntryPoints(ctx context.Context) []*Manifest {
	return DiscoverEntrypointManifests(ctx)
}
