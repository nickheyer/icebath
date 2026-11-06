package pyfs

import (
	"os"
	"path/filepath"
	"testing"
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
