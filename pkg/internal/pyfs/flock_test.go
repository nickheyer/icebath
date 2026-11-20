package pyfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockFileExcludesASecondHandleUntilUnlocked(t *testing.T) {
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
	if err := LockFile(first); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- LockFile(second) }()
	select {
	case err := <-acquired:
		t.Fatalf("second handle locked while the first held the lock: %v", err)
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
		t.Fatal("second handle never acquired the released lock")
	}
	if err := UnlockFile(second); err != nil {
		t.Fatal(err)
	}
}
