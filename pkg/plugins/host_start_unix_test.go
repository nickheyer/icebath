//go:build !windows

package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Home whose host launcher backgrounds a grandchild, records its pid and never answers hello
func hungHostHome(t *testing.T) (string, string) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(root, "grandchild.pid")
	script := "sleep 300 & echo $! > " + pidFile + "; exec sleep 300"
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n  host:\n    launcher: [/bin/sh, -c, '"+script+"', launcher]\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	return h, pidFile
}

// Waits until the pid is gone or a zombie awaiting its reaper
func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) || proc.Zombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d survived the shutdown", pid)
}

func startWaiters(h *PluginHost, ctx context.Context, n int) chan error {
	out := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := h.ensureStarted(ctx)
			out <- err
		}()
	}
	return out
}

func expectReleased(t *testing.T, results chan error, n int, want string) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case err := <-results:
			var unavailable *PluginHostUnavailable
			if !errors.As(err, &unavailable) || !strings.HasPrefix(unavailable.Reason, "plugin host failed to start") || !strings.HasSuffix(unavailable.Reason, want) {
				t.Fatalf("waiter %d got %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiter %d still blocked after the shutdown", i)
		}
	}
}

func TestShutdownKillsAHostThatNeverAnswersHelloAndReleasesEveryWaiter(t *testing.T) {
	h, pidFile := hungHostHome(t)
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return "python3", nil }
	t.Cleanup(func() { hostInterpreter = previous })
	host, err := newPluginHost(NewManager(h))
	if err != nil {
		t.Fatal(err)
	}
	results := startWaiters(host, homeCtx(h), 3)
	var grandchild int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (grandchild == 0 || host.PID() == 0) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			grandchild, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	leader := host.PID()
	if grandchild == 0 || leader == 0 {
		t.Fatalf("hung child never started (leader %d, grandchild %d)", leader, grandchild)
	}
	began := time.Now()
	host.Shutdown()
	if took := time.Since(began); took >= hostShutdownGrace {
		t.Fatalf("shutdown waited %s behind the hung start", took)
	}
	expectReleased(t, results, 3, "): shutdown")
	waitProcessGone(t, leader)
	waitProcessGone(t, grandchild)
	if host.Alive() || host.PID() != 0 {
		t.Fatal("host still reports a live child after the shutdown")
	}
}

func TestShutdownReleasesWaitersOfAStartStillResolvingItsInterpreter(t *testing.T) {
	h, pidFile := hungHostHome(t)
	resolving, unblock := make(chan struct{}), make(chan struct{})
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) {
		close(resolving)
		<-unblock
		return "python3", nil
	}
	t.Cleanup(func() { hostInterpreter = previous })
	host, err := newPluginHost(NewManager(h))
	if err != nil {
		t.Fatal(err)
	}
	results := startWaiters(host, homeCtx(h), 2)
	<-resolving
	began := time.Now()
	host.Shutdown()
	if took := time.Since(began); took >= hostShutdownGrace {
		t.Fatalf("shutdown waited %s behind the interpreter lookup", took)
	}
	expectReleased(t, results, 2, "plugin host failed to start: shutdown")
	close(unblock)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if grandchild, _ := strconv.Atoi(strings.TrimSpace(string(raw))); grandchild != 0 {
				waitProcessGone(t, grandchild)
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if host.Alive() || host.PID() != 0 {
		t.Fatal("an abandoned start installed its child")
	}
}
