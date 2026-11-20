//go:build !windows

package cron

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBoundedJobsLockLockAcquisitionTimesOutAndDegrades(t *testing.T) {
	s, buf := newTestStore(t)
	if err := s.ensureDirs(context.Background()); err != nil {
		t.Fatal(err)
	}
	shortLockTimeout(t, time.Second)
	f, err := os.OpenFile(filepath.Join(s.cronDir, ".jobs.lock"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("test holder failed to take the flock: %v", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	start := time.Now()
	entered := false
	buf.Reset()
	if err := s.withJobsLock(context.Background(), func(tx *jobsTx) error { entered = true; return nil }); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	if !entered {
		t.Fatal("critical section must still run in degraded mode")
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("lock wait was not bounded (took %.1fs)", elapsed.Seconds())
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("degraded-mode fallback must be logged at ERROR, got %q", buf.String())
	}
}
