//go:build !windows

package pyfs

import (
	"os"
	"path/filepath"
	"testing"
)

// Mode capture keeps the setuid bit and the restore helpers apply exactly what was captured
func TestPreserveAndRestoreFileMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if _, ok := PreserveFileMode(path); ok {
		t.Fatal("missing file reported a mode")
	}
	if PreserveFileOwner(path) != nil {
		t.Fatal("missing file reported an owner")
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	RestoreFileMode(path, 0o4755, true)
	mode, ok := PreserveFileMode(path)
	if !ok || (mode != 0o4755 && mode != 0o755) {
		t.Fatalf("mode %o %v", mode, ok)
	}
	RestoreFileMode(path, 0o600, false)
	if again, _ := PreserveFileMode(path); again != mode {
		t.Fatalf("restore without a mode changed it to %o", again)
	}
	owner := PreserveFileOwner(path)
	if owner == nil || owner.UID != os.Getuid() {
		t.Fatalf("owner %+v", owner)
	}
	RestoreFileOwner(path, owner)
	RestoreFileOwner(path, nil)
}
