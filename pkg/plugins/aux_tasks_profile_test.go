package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// Writes a host plugin whose register makes the one given context call
func writeAuxPlugin(t *testing.T, h, name, call string) {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "plugin.yaml"), `{"name": "`+name+`", "version": "0.1.0", "description": "`+name+` probe"}`)
	writeFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    ctx."+call+"\n")
}
