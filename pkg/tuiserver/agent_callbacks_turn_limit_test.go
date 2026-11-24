package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Context on a fresh home whose config.yaml holds the given text
func turnLimitHome(t *testing.T, yaml string) context.Context {
	t.Helper()
	h := t.TempDir()
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return home.WithOverride(context.Background(), h)
}
