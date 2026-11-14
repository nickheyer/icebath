//go:build unix

package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Swaps the guard capability for the test and restores the platform one afterwards
func withLockGuard(t *testing.T, ops *lockGuardOps) {
	t.Helper()
	prev := lockGuard
	lockGuard = ops
	t.Cleanup(func() { lockGuard = prev })
}

func TestGuardDegradesToANoOpWithoutAUsableFcntl(t *testing.T) {
	ofd := 37
	shapes := map[string]*lockGuardOps{
		"absent":         nil,
		"partial":        newLockGuardOps(nil, 6, 0, 2, func(int, int, int16, int64, int64) error { return nil }),
		"constants-only": newLockGuardOps(&ofd, 6, 0, 2, nil),
	}
	for name, ops := range shapes {
		t.Run(name, func(t *testing.T) {
			if ops != nil {
				t.Fatal("partial capability armed the guard")
			}
			withLockGuard(t, ops)
			db := filepath.Join(t.TempDir(), "state.db")
			f, err := os.OpenFile(db, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if lockGuardSupported() {
				t.Fatal("guard reports supported")
			}
			if held := lockGuardHold(db, nil); len(held) != 0 {
				t.Fatalf("held = %v", held)
			}
			lockGuardRelease(lockGuardHeld{})
		})
	}
}

func TestAUsableFcntlStillArmsTheGuard(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("kernel without OFD locks")
	}
	if !lockGuardSupported() {
		t.Fatal("platform guard disarmed")
	}
}
