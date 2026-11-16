package app

import (
	"os"
	"path/filepath"
	"testing"
)

// Text of a collapsed paste reference to a file holding the content
func pasteReference(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return "[Pasted text #1: 2 lines → " + path + "]"
}
