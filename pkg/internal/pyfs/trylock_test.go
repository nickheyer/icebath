package pyfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLockFileFailsWhileAnotherHandleHoldsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if ok, err := TryLockFile(first); !ok || err != nil {
		t.Fatalf("first lock %v %v", ok, err)
	}
	if ok, _ := TryLockFile(second); ok {
		t.Fatal("second handle locked while the first held the lock")
	}
	if err := UnlockFile(first); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLockFile(second); !ok || err != nil {
		t.Fatalf("second lock after unlock %v %v", ok, err)
	}
}

func TestLockFileWaitsUntilTheHolderUnlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot")
	first, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := LockFile(first); err != nil {
		t.Fatal(err)
	}
	second, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if ok, err := TryLockFile(second); ok || err != nil {
		t.Fatalf("second handle locked a held file: ok %v err %v", ok, err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- LockFile(second) }()
	select {
	case err := <-acquired:
		t.Fatalf("blocking lock returned while the first handle held it: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := UnlockFile(first); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocking lock never acquired after the holder unlocked")
	}
}
