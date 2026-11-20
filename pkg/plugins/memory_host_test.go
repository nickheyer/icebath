package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

// Writes body to path creating its parent directories
func memWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, body)
}
