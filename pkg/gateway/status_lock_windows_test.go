//go:build windows

package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsFileLockUsesHighOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !tryAcquireFileLock(f) {
		t.Fatal("lock refused")
	}
	other, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	low := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(other.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, low); err != nil {
		t.Fatal("byte zero locked, readers of the record would block")
	}
	windows.UnlockFileEx(windows.Handle(other.Fd()), 0, 1, 0, low)
	if tryAcquireFileLock(other) {
		t.Fatal("high offset byte not locked")
	}
	releaseFileLock(f)
	if data, _ := os.ReadFile(path); string(data) != "\n" {
		t.Fatalf("content %q", data)
	}
}
