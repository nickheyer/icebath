package home

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestRmtreeForceRemovesReadOnlyFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "clone")
	obj := filepath.Join(root, ".git", "objects", "ab", "cdef")
	writeText(t, obj, "blob")
	if err := os.Chmod(obj, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := RmtreeForce(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("tree survived")
	}
}

func TestRmtreeForceReportsWhatStillRefuses(t *testing.T) {
	if os.Getuid() == 0 || !stuckTrackingSupported() {
		t.Skip("needs a non-root POSIX user, root deletes through a read-only dir")
	}
	root := filepath.Join(t.TempDir(), "tree")
	sealed := filepath.Join(root, "sealed")
	writeText(t, filepath.Join(sealed, "f"), "x")
	if err := os.Chmod(sealed, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o700); _ = os.Chmod(filepath.Join(sealed, "f"), 0o600) })
	if err := RmtreeForce(root); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("RmtreeForce = %v, want a permission error", err)
	}
	if err := RmtreeForce(filepath.Join(root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing tree gave %v", err)
	}
}
