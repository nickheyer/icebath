package pm

import (
	"path/filepath"
	"testing"
)

func TestExportedSwapLockfilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock.json")
	previous := SwapLockfilePath(func() string { return path })
	got := LockfilePath()
	SwapLockfilePath(previous)
	if got != path {
		t.Fatalf("swapped lockfile %q", got)
	}
}
