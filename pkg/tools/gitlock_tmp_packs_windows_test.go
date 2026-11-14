//go:build windows

package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestWindowsReadonlyDebrisIsCleared(t *testing.T) {
	repo, pack := glPackRepo(t)
	glGuard(t, false)
	debris := filepath.Join(pack, "tmp_pack_ReadOnly")
	if err := os.WriteFile(debris, bytes.Repeat([]byte("x"), 128), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-glStalePack)
	if err := os.Chtimes(debris, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(debris, 0o444); err != nil {
		t.Fatal(err)
	}
	if removed := ClearStaleTmpPacks(repo, nil); !slices.Equal(removed, []string{debris}) {
		t.Fatalf("removed %v", removed)
	}
	if pathExists(debris) {
		t.Fatal("read only debris survived the sweep")
	}
}
