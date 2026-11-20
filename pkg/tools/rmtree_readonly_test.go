package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRmtreeReadonlyRemovesReadOnlyObjectInReadOnlyDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins", "demo")
	objDir := filepath.Join(root, ".git", "objects", "4b")
	if err := os.MkdirAll(objDir, 0o755); err != nil {
		t.Fatal(err)
	}
	obj := filepath.Join(objDir, "825dc642cb6eb9a060e54bf8d69288fbee4904")
	if err := os.WriteFile(obj, []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chmod(obj, 0o444)
	os.Chmod(objDir, 0o555)
	t.Cleanup(func() { os.Chmod(objDir, 0o755) })
	if err := RmtreeReadonly(root, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root still exists: %v", err)
	}
}

func TestRmtreeReadonlyMissingPathRaisesUnlessIgnored(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	if err := RmtreeReadonly(missing, false); !os.IsNotExist(err) {
		t.Fatalf("missing root must raise FileNotFoundError, got %v", err)
	}
	if err := RmtreeReadonly(missing, true); err != nil {
		t.Fatalf("ignore errors still returned %v", err)
	}
}
