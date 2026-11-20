//go:build windows

package kanban

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// One LK_NBLCK style attempt on the first byte of the init lock file from a separate handle
func tryByteRangeLock(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var ov windows.Overlapped
	h := windows.Handle(f.Fd())
	if windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov) != nil {
		return false
	}
	windows.UnlockFileEx(h, 0, 1, 0, &ov)
	return true
}

func TestCrossProcessInitLockUsesWindowsByteRangeLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kanban.db")
	release, err := crossProcessInitLock(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if tryByteRangeLock(t, dbPath+".init.lock") {
		release()
		t.Fatal("first byte of the init lock not held while the lock is taken")
	}
	release()
	if !tryByteRangeLock(t, dbPath+".init.lock") {
		t.Fatal("first byte of the init lock still held after release")
	}
}
