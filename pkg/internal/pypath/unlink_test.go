package pypath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnlinkRemovesFilesAndRefusesDirectories(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(file); err != nil {
		t.Fatalf("Unlink(file) = %v", err)
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
	if err := Unlink(file); OSErrorClass(err) != "FileNotFoundError" {
		t.Fatalf("Unlink(missing) = %v, class %s", err, OSErrorClass(err))
	}
	sub := filepath.Join(dir, "empty")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Unlink(sub); err == nil {
		t.Fatal("Unlink removed an empty directory")
	}
	if st, err := os.Stat(sub); err != nil || !st.IsDir() {
		t.Fatalf("directory gone after a refused unlink: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := Unlink(link); err != nil {
		t.Fatalf("Unlink(dir symlink) = %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link still present: %v", err)
	}
}
