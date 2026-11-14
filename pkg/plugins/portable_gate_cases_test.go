package plugins_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Portable plugin whose worker server requires an executable app at the given path on one OS
func writePortableAppPlugin(t *testing.T, root, app, osKey string) {
	t.Helper()
	if osKey == "" {
		osKey = hostinfo.OSFamily()
	}
	manifest, _ := json.Marshal(map[string]any{"$schema": plugins.PluginSchemaV1, "name": "example-plugin",
		"extensions": map[string]any{"com.nousresearch.hermes": map[string]any{"servers": map[string]any{"worker": map[string]any{
			"app":      map[string]any{osKey: map[string]any{"presence": "executable", "location": app}},
			"requires": map[string]any{"app": true},
		}}}}})
	servers, _ := json.Marshal(map[string]any{"$schema": plugins.MCPSchemaV1,
		"mcpServers": map[string]any{"worker": map[string]any{"type": "stdio", "command": "python"}}})
	for name, data := range map[string][]byte{"plugin.json": manifest, "mcp.json": servers} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Installs the real server availability service for the duration of the case
func realAvailability(t *testing.T) {
	t.Helper()
	previous := plugins.InstalledServices()
	next := plugins.Services{}
	if previous != nil {
		next = *previous
	}
	next.ServerAvailabilityReason = mcp.ServerAvailabilityReason
	plugins.InstallServices(&next)
	t.Cleanup(func() { plugins.InstallServices(previous) })
}

func TestPortableInstallGateAcceptsPresentAppAndRefusesMissing(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	realAvailability(t)
	tree := t.TempDir()
	app := filepath.Join(tree, "example-app")
	if err := os.WriteFile(app, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	writePortableAppPlugin(t, tree, app, "")
	if err := plugins.RefuseUnavailablePortablePlugin(context.Background(), "example-plugin", tree, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(app); err != nil {
		t.Fatal(err)
	}
	err := plugins.RefuseUnavailablePortablePlugin(context.Background(), "example-plugin", tree, "")
	var opErr *plugins.PluginOperationError
	if !errors.As(err, &opErr) || !regexp.MustCompile(`example-plugin.*worker is not installed. Install worker`).MatchString(err.Error()) {
		t.Fatalf("missing app %v", err)
	}
}

func TestPortableInstallGateOnUndeclaredOsDoesNotSayInstall(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	realAvailability(t)
	other := ""
	for _, family := range []string{"linux", "darwin", "win32"} {
		if family != hostinfo.OSFamily() && other == "" {
			other = family
		}
	}
	tree := t.TempDir()
	writePortableAppPlugin(t, tree, filepath.Join(tree, "example-app"), other)
	err := plugins.RefuseUnavailablePortablePlugin(context.Background(), "example-plugin", tree, "Example App")
	var opErr *plugins.PluginOperationError
	if !errors.As(err, &opErr) || !strings.Contains(err.Error(), "Example App is not supported on this operating system") ||
		strings.Contains(err.Error(), "Install") {
		t.Fatalf("undeclared os %v", err)
	}
}
