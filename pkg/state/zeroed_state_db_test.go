package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsZeroedStateDBAndQuarantine(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if !IsZeroedStateDB(db, 100, false) {
		t.Fatal("a NUL filled file is zeroed")
	}
	q, err := QuarantineInvalidStateDB(db, false)
	if err != nil || q == "" {
		t.Fatalf("quarantine = %q %v", q, err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatal("the zeroed file left its path")
	}
	if got := mustRead(t, q); !bytes.Equal(got, make([]byte, 1024)) {
		t.Fatal("the quarantined copy keeps its bytes")
	}
}

func TestQuarantineFailsClosedWhenLockHeld(t *testing.T) {
	db := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(db, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := db + ".quarantine.lock"
	held, release, done := make(chan error, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		handle, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			held <- err
			return
		}
		defer handle.Close()
		if err := quarantineTryLock(handle); err != nil {
			held <- err
			return
		}
		held <- nil
		select {
		case <-release:
		case <-time.After(15 * time.Second):
		}
		quarantineUnlock(handle)
	}()
	if err := <-held; err != nil {
		t.Fatalf("background holder failed to acquire the lock: %v", err)
	}
	result, err := QuarantineInvalidStateDB(db, false)
	close(release)
	<-done
	if err != nil || result != "" {
		t.Fatalf("quarantine under a held lock = %q %v, want fail closed", result, err)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatal("the zeroed state.db was moved despite the held lock")
	}
	if !IsZeroedStateDB(db, 100, false) {
		t.Fatal("the file is still zeroed")
	}
}
