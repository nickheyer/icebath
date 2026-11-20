package provider

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFileLockIsReentrantAndStampsTheHolderPid(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "nested", "queue.jsonl.lock")
	release, err := FileLock(lockPath, time.Second, "timed out")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := FileLock(lockPath, time.Second, "timed out")
	if err != nil {
		t.Fatalf("reentry from the holding goroutine failed: %v", err)
	}
	inner()
	raw, _ := os.ReadFile(lockPath)
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("held lock file %q", raw)
	}
	release()
	raw, _ = os.ReadFile(lockPath)
	if strings.TrimSpace(string(raw)) != "" {
		t.Fatalf("released lock file still names a holder %q", raw)
	}
	again, err := FileLock(lockPath, time.Second, "timed out")
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestFileLockWaiterTimesOutWithTheCallerMessageWhileAnotherGoroutineHolds(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "say_queue.jsonl.lock")
	release, err := FileLock(lockPath, time.Second, "unused")
	if err != nil {
		t.Fatal(err)
	}
	msg := "Timed out waiting for say queue lock (" + lockPath + ")"
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		rel, err := FileLock(lockPath, 10*time.Millisecond, msg)
		if rel != nil {
			rel()
		}
		done <- err
	}()
	err = <-done
	var te *FileLockTimeoutError
	if !errors.As(err, &te) || te.Error() != msg {
		t.Fatalf("waiter got %v", err)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("deadline floor of one second not applied, waited %v", waited)
	}
	acquired := make(chan error, 1)
	go func() {
		rel, err := FileLock(lockPath, 5*time.Second, msg)
		if rel != nil {
			rel()
		}
		acquired <- err
	}()
	time.Sleep(100 * time.Millisecond)
	release()
	if err := <-acquired; err != nil {
		t.Fatalf("waiter after release got %v", err)
	}
}
