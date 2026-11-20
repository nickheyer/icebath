package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Gateway lock dir readable and listable but closed to writes, restored when the test ends
func lockedOutDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	dir := filepath.Join(t.TempDir(), "gateway-locks")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o500)
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", dir)
	ResetUnusableLockDirReports()
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	return dir
}

func TestLockDirRepairIsPickedUpWithoutRestart(t *testing.T) {
	dir := lockedOutDir(t)
	_, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "tok", nil)
	var unusable *ScopedLockDirUnusable
	if !errors.As(err, &unusable) {
		t.Fatalf("want ScopedLockDirUnusable, got %v", err)
	}
	os.Chmod(dir, 0o700)
	acquired, existing, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "tok", nil)
	if err != nil || !acquired || existing != nil {
		t.Fatalf("after repair %v %v %v", acquired, existing, err)
	}
	if UnusableLockDirReported(dir) {
		t.Fatal("repaired lock dir still reported unusable")
	}
	ReleaseScopedLock("telegram-bot-token", "tok")
}

func TestUnreadableStaleTokenLockIsNamedNotReportedAsAHolderlessConflict(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits")
	}
	dir := filepath.Join(t.TempDir(), "gateway-locks")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", dir)
	stale := scopeLockPath("telegram-bot-token", "tok")
	writeBytes(t, stale, `{"pid": 1}`)
	os.Chmod(stale, 0)
	os.Chmod(dir, 0o500)
	defer func() {
		os.Chmod(dir, 0o700)
		os.Chmod(stale, 0o600)
	}()
	_, _, err := AcquireScopedLock(context.Background(), "telegram-bot-token", "tok", nil)
	var unusable *ScopedLockDirUnusable
	if !errors.As(err, &unusable) {
		t.Fatalf("want ScopedLockDirUnusable, got %v", err)
	}
}
