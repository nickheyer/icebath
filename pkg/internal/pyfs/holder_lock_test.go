package pyfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHolderFileLockReentersStampsAndTimesOut(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "sub", ".env.lock")
	var holder LockHolder
	release, err := HolderFileLock(lockPath, &holder, time.Second, "Timed out waiting for the environment file lock (x)")
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := os.ReadFile(lockPath)
	if string(stamp) != fmt.Sprintf("%d\n", os.Getpid()) {
		t.Fatalf("pid stamp %q", stamp)
	}
	inner, err := HolderFileLock(lockPath, &holder, time.Second, "unused")
	if err != nil || holder.Depth() != 2 {
		t.Fatalf("reentry depth %d err %v", holder.Depth(), err)
	}
	inner()
	if holder.Depth() != 1 {
		t.Fatalf("unwound depth %d", holder.Depth())
	}
	var other LockHolder
	done := make(chan error, 1)
	go func() {
		_, err := HolderFileLock(lockPath, &other, 10*time.Millisecond, "Timed out waiting for the environment file lock (x)")
		done <- err
	}()
	err = <-done
	var timeout *LockTimeoutError
	if !errors.As(err, &timeout) || timeout.Message != "Timed out waiting for the environment file lock (x)" {
		t.Fatalf("own pid gives no hint %v", err)
	}
	release()
	if holder.Depth() != 0 {
		t.Fatalf("released depth %d", holder.Depth())
	}
	if after, _ := os.ReadFile(lockPath); string(after) != holderReleasePad {
		t.Fatalf("released file %q", after)
	}
	again, err := HolderFileLock(lockPath, &other, time.Second, "unused")
	if err != nil {
		t.Fatalf("free lock %v", err)
	}
	again()
}

func TestLockHolderHintNamesOnlyALiveForeignPid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.lock")
	for _, text := range []string{"", "x\n", "0\n", fmt.Sprintf("%d\n", os.Getpid())} {
		os.WriteFile(path, []byte(text), 0o600)
		if hint := LockHolderHint(path); hint != "" {
			t.Fatalf("%q gave %q", text, hint)
		}
	}
	if LockHolderHint(filepath.Join(t.TempDir(), "missing")) != "" {
		t.Fatal("missing file gives no hint")
	}
	os.WriteFile(path, []byte("\uFEFF1 trailing"), 0o600)
	if hint := LockHolderHint(path); !strings.HasPrefix(hint, "another icebath process (pid 1)") || hint != "another icebath process (pid 1) probably still holds it" {
		t.Fatalf("live pid 1 %q", hint)
	}
}
