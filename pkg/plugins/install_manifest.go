package plugins

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Whether plugin.json exists under a plugin dir, a dangling symlink included
func hasPortableManifest(pluginDir string) bool {
	_, err := os.Lstat(filepath.Join(pluginDir, "plugin.json"))
	return err == nil
}

// Manifest an install candidate publishes, a manifest_invalid error when it is unreadable or malformed, empty without one
func ReadManifestForInstall(pluginDir string) (map[string]any, error) {
	native, err := pm.NativeManifestFile(pluginDir)
	if err != nil {
		return nil, &PluginOperationError{Msg: err.Error(), FailureClass: "manifest_invalid"}
	}
	if native != "" {
		manifest, err := pm.ReadNativeManifest(native)
		if err != nil {
			return nil, &PluginOperationError{Msg: fmt.Sprintf("Could not read plugin manifest %s: %v", native, err),
				FailureClass: "manifest_invalid"}
		}
		if manifest == nil {
			return nil, &PluginOperationError{Msg: "Plugin manifest must be a mapping: " + native, FailureClass: "manifest_invalid"}
		}
		return manifest, nil
	}
	if !hasPortableManifest(pluginDir) {
		return map[string]any{}, nil
	}
	manifest, diagnostics, err := ReadAgentPluginManifest(pluginDir)
	if err != nil {
		return nil, &PluginOperationError{Msg: "Portable plugin manifest validation failed: " + err.Error(),
			FailureClass: "manifest_invalid"}
	}
	for _, d := range diagnostics {
		logWarn("Agent Plugin install: " + d.Message)
	}
	return manifest, nil
}
