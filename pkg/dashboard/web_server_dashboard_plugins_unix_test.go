//go:build unix

package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDashboardPluginManifestExtensionsUnreadablePluginPathsDoNotBlockDiscovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("permission denial needs a non root user")
	}
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	writeHomePlugin(t, tmp, "valid", map[string]any{"name": "valid", "label": "Valid Plugin", "entry": "dist/index.js"})
	deniedRoot := filepath.Join(tmp, "denied-root")
	deniedDashboard := filepath.Join(tmp, "plugins", "denied", "dashboard")
	for _, d := range []string{deniedRoot, deniedDashboard} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(deniedDashboard, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{deniedRoot, deniedDashboard} {
		if err := os.Chmod(d, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	}
	search := append([]pluginSearchDir{{deniedRoot, "user", false}}, dashboardPluginSearchDirs()...)
	list := discoverDashboardPlugins(search)
	if len(entriesNamed(list, "valid")) != 1 {
		t.Fatal("valid plugin blocked by an unreadable path")
	}
	if len(entriesNamed(list, "denied")) != 0 {
		t.Fatal("denied plugin listed")
	}
}
