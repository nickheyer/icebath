package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadManifestForInstallStopsOnMalformedManifests(t *testing.T) {
	dir := t.TempDir()
	if got, err := ReadManifestForInstall(dir); err != nil || len(got) != 0 {
		t.Fatalf("no manifest %v %v", got, err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: demo\nversion: 1.0.0\n")
	if got, err := ReadManifestForInstall(dir); err != nil || got["name"] != "demo" {
		t.Fatalf("native %v %v", got, err)
	}
	for _, body := range []string{"- a\n- list\n", "name: [unclosed\n"} {
		writeFile(t, filepath.Join(dir, "plugin.yaml"), body)
		_, err := ReadManifestForInstall(dir)
		var op *PluginOperationError
		if !errors.As(err, &op) || op.FailureClass != "manifest_invalid" ||
			!strings.HasPrefix(op.Msg, "Could not read plugin manifest "+filepath.Join(dir, "plugin.yaml")) {
			t.Fatalf("%q gave %v", body, err)
		}
	}
	portable := t.TempDir()
	if err := os.Symlink(filepath.Join(portable, "missing.json"), filepath.Join(portable, "plugin.json")); err != nil {
		t.Fatal(err)
	}
	_, err := ReadManifestForInstall(portable)
	var op *PluginOperationError
	if !errors.As(err, &op) || op.FailureClass != "manifest_invalid" || !strings.HasPrefix(op.Msg, "Portable plugin manifest validation failed: ") {
		t.Fatalf("dangling portable manifest gave %v", err)
	}
}
