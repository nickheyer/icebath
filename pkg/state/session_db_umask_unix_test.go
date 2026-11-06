//go:build unix

package state

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWritableStateDBIsOwnerOnlyUnderPermissiveUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	old := syscall.Umask(0o022)
	db, err := NewSessionDB(t.Context(), path, false)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var seen int
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		seen++
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", p, st.Mode().Perm())
		}
	}
	if seen == 0 {
		t.Fatal("no state files")
	}
}
