//go:build !windows

package browser

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch through the real launcher of a fake that also forks a helper, returning the fake and the test ledger
func leakedLaunch(t *testing.T, key string, extra ...string) (int, string) {
	env := launcherTestEnv(t)
	exe := fakeExe(t, "order-chromium", extra...)
	pin(t, &launcherResolveExecutable, func(*tools.Env) string { return exe })
	b, err := launchEphemeral(env, key, LaunchOptions{})
	if err != "" || b == nil || b.PID == nil {
		t.Fatalf("launch %s: %s", key, err)
	}
	ledger := childLedger(t)
	waitUntil(t, 15*time.Second, "the fake and its helper in the ledger", func() bool { return len(ledgerLive(ledger)) == 2 })
	launcherMu.Lock()
	browsers.del(key)
	launcherMu.Unlock()
	return int(*b.PID), ledger
}

func processGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// A launched fake the launcher forgot, so no stop reaches it, is gone with its group once its test cleanup ran
func TestHarnessCleanupEndsEveryChild(t *testing.T) {
	var pid int
	var noted []int
	t.Run("leak", func(t *testing.T) {
		var ledger string
		pid, ledger = leakedLaunch(t, "leak")
		noted = ledgerLive(ledger)
	})
	waitUntil(t, 15*time.Second, "the reaper of the killed fake", func() bool { return processGone(pid) })
	if !processGone(-pid) {
		t.Fatalf("process group %d survived the cleanup", pid)
	}
	for _, n := range noted {
		if !processGone(n) {
			t.Fatalf("child %d survived the cleanup", n)
		}
	}
}

// A launched fake and its helper exit on their own once the root pipe they watch loses its writer
func TestChildModesExitWhenRootPipeCloses(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "root")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, ledger := leakedLaunch(t, "orphan", rootPipeEnv+"='"+fifo+"'")
	noted := ledgerLive(ledger)
	writer.Close()
	waitUntil(t, 15*time.Second, "every child to exit with its root pipe", func() bool { return len(ledgerLive(ledger)) == 0 })
	for _, n := range noted {
		waitUntil(t, 15*time.Second, "the exited child to be reaped", func() bool { return processGone(n) })
	}
}
