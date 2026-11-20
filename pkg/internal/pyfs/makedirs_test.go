package pyfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Missing parents are created, an existing directory passes only with exist_ok and a file in the way fails
func TestMakedirsLikeOsMakedirs(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := Makedirs(deep, 0o777, false); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(deep); err != nil || !st.IsDir() {
		t.Fatalf("not created: %v", err)
	}
	if err := Makedirs(deep, 0o777, true); err != nil {
		t.Fatalf("exist_ok: %v", err)
	}
	if err := Makedirs(deep, 0o777, false); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing without exist_ok: %v", err)
	}
	if err := Makedirs(filepath.Join(root, "x", "y")+"/", 0o777, false); err != nil {
		t.Fatalf("trailing slash: %v", err)
	}
	if err := Makedirs(filepath.Join(root, "p", "."), 0o777, false); err != nil {
		t.Fatalf("dot tail: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "p")); err != nil || !st.IsDir() {
		t.Fatalf("dot tail parent: %v", err)
	}
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Makedirs(file, 0o777, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("file with exist_ok: %v", err)
	}
	if err := Makedirs(filepath.Join(file, "sub"), 0o777, true); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("file parent: %v", err)
	}
}
