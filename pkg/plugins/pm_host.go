package plugins

import "github.com/nickheyer/icebath/pkg/pm"

// Installs the manifest surface pkg/pm reads for plugin declarations, manifest versions and fit checks
func init() {
	pm.SetPluginManifestHost(&pm.PluginManifestHost{
		SupportedManifestVersion: SupportedManifestVersion,
		RunningVersion:           func() string { return RunningVersion },
		RequiresError: func(manifest map[string]any) string {
			return RequiresVersionError(scalarString(manifest["requires_icebath"]))
		},
		ReadAgentManifest: func(pluginDir string) (map[string]any, error) {
			manifest, _, err := ReadAgentPluginManifest(pluginDir)
			return manifest, err
		},
	})
}
