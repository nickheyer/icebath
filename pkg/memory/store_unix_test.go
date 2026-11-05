//go:build !windows

package memory

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestMemoryFileLockNewLockFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	old := syscall.Umask(0o002)
	unlock, err := fileLock(filepath.Join(dir, "MEMORY.md"))
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	st, _ := os.Stat(filepath.Join(dir, "MEMORY.md.lock"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestMemoryFileLockExistingLooseLockIsTightened(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "MEMORY.md.lock")
	os.WriteFile(lockPath, nil, 0o664)
	os.Chmod(lockPath, 0o664)
	unlock, err := fileLock(filepath.Join(dir, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	st, _ := os.Stat(lockPath)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestMemoryFileLockSymlinkIsRefused(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	os.WriteFile(outside, []byte("do not touch"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "MEMORY.md.lock"))
	if _, err := fileLock(filepath.Join(dir, "MEMORY.md")); err == nil {
		t.Fatal("symlinked lock accepted")
	}
	if b, _ := os.ReadFile(outside); string(b) != "do not touch" {
		t.Fatalf("outside changed: %q", b)
	}
}
