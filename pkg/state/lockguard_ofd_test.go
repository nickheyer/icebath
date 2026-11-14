//go:build linux || darwin

package state

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

// Tries an exclusive OFD lock over the shared range through a separate open file description
func foreignExclusiveRefused(t *testing.T, db string) bool {
	t.Helper()
	f, err := os.OpenFile(db, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = unixFcntlFlock(int(f.Fd()), lockGuard.ofdSetLK, unix.F_WRLCK, sharedFirst, sharedSize)
	if err == nil {
		_ = unixFcntlFlock(int(f.Fd()), lockGuard.ofdSetLK, unix.F_UNLCK, sharedFirst, sharedSize)
		return false
	}
	return true
}

func TestHoldSurvivesAStrayCloseUntilTheLastHandleReleases(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OFD conflict semantics exercised on Linux")
	}
	db := filepath.Join(t.TempDir(), "state.db")
	conn, err := os.OpenFile(db, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	first := lockGuardHold(db, nil)
	second := lockGuardHold(db, nil)
	ident, _ := statIdentity(db)
	if first[ident] != (lockRange{sharedFirst, sharedSize}) || lockGuardHandles[ident] != 2 {
		t.Fatalf("held = %v handles = %d", first, lockGuardHandles[ident])
	}
	if again := lockGuardHold(db, first); lockGuardHandles[ident] != 2 || len(again) != 1 {
		t.Fatalf("re-hold counted twice: %d", lockGuardHandles[ident])
	}
	stray, err := os.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	stray.Close()
	if !foreignExclusiveRefused(t, db) {
		t.Fatal("stray close cancelled the guard")
	}
	lockGuardRelease(first)
	if len(first) != 0 || !foreignExclusiveRefused(t, db) {
		t.Fatal("first release unlocked while a handle remains")
	}
	lockGuardRelease(second)
	if _, ok := lockGuardHandles[ident]; ok {
		t.Fatal("last release kept the handle count")
	}
	if !foreignExclusiveRefused(t, db) {
		t.Fatal("last release left the open connection unlocked")
	}
	conn.Close()
	if foreignExclusiveRefused(t, db) {
		t.Fatal("closing the connection kept a lock")
	}
}
