//go:build !windows

package plugins

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// An allowlist write waits on a held sibling flock and releases it once the update lands
func TestShellHookAllowlistFlockSerialisesAndUnlocks(t *testing.T) {
	shellHookWorld(t)
	ctx := context.Background()
	lockPath := ShellHookAllowlistPath(ctx) + ".lock"
	if err := recordShellHookApproval(ctx, "pre_tool_call", "/hooks/a.sh"); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("lock file missing after an approval %v", err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("approval left the allowlist flock held %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := RevokeShellHook(ctx, "/hooks/a.sh")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("revoke ran while another holder had the flock %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if n := len(AllowlistedShellHookPairs(ctx)); n != 1 {
		t.Fatalf("revoke wrote under a held flock, %d approvals left", n)
	}
	flockUnlock(holder)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(AllowlistedShellHookPairs(ctx)); n != 0 {
		t.Fatalf("revoke after unlock kept %d approvals", n)
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("revoke left the allowlist flock held %v", err)
	}
}
