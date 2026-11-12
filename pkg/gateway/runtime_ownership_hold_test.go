package gateway

import (
	"context"
	"path/filepath"
	"testing"
)

// Whether a fresh descriptor can take the gateway.lock of a home right now, releasing it again
func lockFree(t *testing.T, h string) bool {
	t.Helper()
	f, err := openLock(filepath.Join(h, gatewayLockFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !tryAcquireFileLock(f) {
		return false
	}
	releaseFileLock(f)
	return true
}

func TestReleaseWaitsForHeldWorkAndRefusesNewWork(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := t.TempDir()
	owner := NewProfileOwnership()
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	leave, held := owner.Hold(h)
	if !held {
		t.Fatal("work refused in a reserved home")
	}
	owner.Release(h)
	if owner.Owns(h) {
		t.Fatal("a released home still reads as owned")
	}
	if lockFree(t, h) {
		t.Fatal("the lock went before the held work ended")
	}
	if _, again := owner.Hold(h); again {
		t.Fatal("work entered a released home")
	}
	leave()
	leave()
	if !lockFree(t, h) {
		t.Fatal("the lock outlived the last held work")
	}
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	second, held := owner.Hold(h)
	if !held {
		t.Fatal("work refused after the home was reserved again")
	}
	second()
	owner.Close()
	if !lockFree(t, h) {
		t.Fatal("close kept the reservation")
	}
}

func TestReserveTakesBackALockParkedForHeldWork(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	h := t.TempDir()
	owner := NewProfileOwnership()
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	leave, _ := owner.Hold(h)
	owner.Release(h)
	if err := owner.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatal(err)
	}
	if !owner.Owns(h) {
		t.Fatal("the parked reservation was not taken back")
	}
	leave()
	if lockFree(t, h) {
		t.Fatal("ending the work dropped a reservation taken back meanwhile")
	}
	if work, held := owner.Hold(h); !held {
		t.Fatal("work refused in a home reserved again")
	} else {
		work()
	}
	owner.Close()
	if !lockFree(t, h) {
		t.Fatal("close kept the reservation")
	}
}

func TestWorkInAHomeNeverReservedIsAdmitted(t *testing.T) {
	owner := NewProfileOwnership()
	leave, held := owner.Hold(t.TempDir())
	if !held {
		t.Fatal("work refused in a home this process never reserved")
	}
	leave()
}
