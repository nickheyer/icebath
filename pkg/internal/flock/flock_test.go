package flock

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockExcludesASecondHolderUntilUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Time, 1)
	go func() {
		second, err := Acquire(path)
		if err != nil {
			t.Error(err)
		}
		got <- time.Now()
		second.Unlock()
	}()
	time.Sleep(60 * time.Millisecond)
	released := time.Now()
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-got:
		if at.Before(released) {
			t.Fatal("second holder acquired the lock before the first released it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second holder never acquired the lock")
	}
	if first.Unlock() != nil {
		t.Fatal("double unlock must be harmless")
	}
	var nilLock *Lock
	if nilLock.Unlock() != nil {
		t.Fatal("nil unlock")
	}
	if _, err := Acquire(filepath.Join(t.TempDir(), "missing", "dir", "x.lock")); err == nil {
		t.Fatal("unopenable path accepted")
	}
}
