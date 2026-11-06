package plugins

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Version floor of a raw manifest, requires_icebath first and the upstream requires_hermes spelling after it
func manifestRequires(manifest map[string]any) string {
	if spec := strings.TrimSpace(scalarString(manifest["requires_icebath"])); spec != "" {
		return spec
	}
	return strings.TrimSpace(scalarString(manifest["requires_hermes"]))
}

// Installs the manifest surface pkg/pm reads for plugin declarations, manifest versions and fit checks
func init() {
	pm.SetPluginManifestHost(&pm.PluginManifestHost{
		SupportedManifestVersion: SupportedManifestVersion,
		RunningVersion:           func() string { return RunningVersion },
		RequiresError:            func(manifest map[string]any) string { return RequiresVersionError(manifestRequires(manifest)) },
		ReadAgentManifest: func(pluginDir string) (map[string]any, error) {
			manifest, _, err := ReadAgentPluginManifest(pluginDir)
			return manifest, err
		},
	})
}
