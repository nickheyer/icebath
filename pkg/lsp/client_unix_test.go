//go:build !windows

package lsp

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
)

func TestCancelledStartHardKillsSigtermIgnoringDescendant(t *testing.T) {
	rt, _, _ := testRuntime(t)
	l := testLoop(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	c := mockClient(t, l, rt, dir, "slow_tree", map[string]string{"MOCK_LSP_CHILD_PID": pidFile})
	childPid := 0
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		var startErr error
		inner := l.spawn(ctx, func(ctx context.Context) {
			_, startErr = waitFor(ctx, 15*time.Second, func(ctx context.Context) (struct{}, error) {
				startCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				watcher := l.spawn(ctx, func(wctx context.Context) {
					for {
						if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
							childPid, _ = strconv.Atoi(string(b))
							cancel()
							return
						}
						if l.sleep(wctx, 10*time.Millisecond) != nil {
							return
						}
					}
				})
				defer watcher.cancel()
				return struct{}{}, c.start(startCtx)
			})
		})
		l.await(ctx, inner.done)
		if !isCancellation(startErr) {
			t.Errorf("start must end cancelled, got %v", startErr)
		}
		return struct{}{}, nil
	})
	must(t, err)
	if childPid == 0 {
		t.Fatal("child never started")
	}
	defer syscall.Kill(childPid, syscall.SIGKILL)
	waitUntil(t, 15*time.Second, "the SIGTERM ignoring child to die", func() bool { return !pidAlive(childPid) })
	l.with(func() {
		if c.state != "error" || c.proc != nil {
			t.Errorf("state %s proc %v", c.state, c.proc)
		}
	})
}

// Whether a pid names a live non zombie process

func TestServerGroupIsOwnedOnSpawnAndReleasedOnShutdown(t *testing.T) {
	rt, _, led := testRuntime(t)
	l := testLoop(t)
	offLoop := true
	prevRegister := rt.RegisterChild
	rt.RegisterChild = func(ctx context.Context, pid int, purpose string) bool {
		if !l.mu.TryLock() {
			offLoop = false
		} else {
			l.mu.Unlock()
		}
		if purpose != "lsp-server" {
			t.Errorf("purpose %q", purpose)
		}
		return prevRegister(ctx, pid, purpose)
	}
	c := mockClient(t, l, rt, t.TempDir(), "clean", nil)
	var pid int
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		if err := c.start(ctx); err != nil {
			return struct{}{}, err
		}
		pid = c.proc.pid
		if pg, _ := syscall.Getpgid(pid); pg != pid {
			t.Errorf("server pgid %d, pid %d", pg, pid)
		}
		return struct{}{}, nil
	})
	must(t, err)
	owned, children, _ := led.snapshot()
	if len(owned) != 1 || owned[0] != pid || len(children) != 1 || children[0] != pid || !offLoop {
		t.Fatalf("owned %v children %v off loop %v", owned, children, offLoop)
	}
	shutdownClient(t, l, c)
	if _, _, released := led.snapshot(); len(released) != 1 || released[0] != pid {
		t.Fatalf("released %v", released)
	}
}

func TestSurvivingGroupMemberStaysOwned(t *testing.T) {
	rt, _, led := testRuntime(t)
	rt.ReleaseProcessGroupOnceEmpty = func(pgid int, _ func(), _ time.Duration) bool {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			led.mu.Lock()
			led.released = append(led.released, pgid)
			led.mu.Unlock()
			return true
		}
		return false
	}
	prevKill, prevGrace := killServerTree, shutdownGrace
	killServerTree = func(*serverProc) {}
	defer func() { killServerTree, shutdownGrace = prevKill, prevGrace }()
	l := testLoop(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	c := mockClient(t, l, rt, dir, "slow_tree", map[string]string{"MOCK_LSP_CHILD_PID": pidFile})
	var leader int
	_, err := onLoop(t, l, func(ctx context.Context) (struct{}, error) {
		err := c.start(ctx)
		if c.proc != nil {
			leader = c.proc.pid
		}
		return struct{}{}, err
	})
	must(t, err)
	waitUntil(t, 15*time.Second, "the SIGTERM ignoring child to start", func() bool {
		b, err := os.ReadFile(pidFile)
		return err == nil && len(b) > 0
	})
	b, _ := os.ReadFile(pidFile)
	child, _ := strconv.Atoi(string(b))
	defer syscall.Kill(child, syscall.SIGKILL)
	defer syscall.Kill(leader, syscall.SIGKILL)
	shutdownGrace = 100 * time.Millisecond
	syscall.Kill(leader, syscall.SIGKILL)
	shutdownClient(t, l, c)
	if _, _, released := led.snapshot(); len(released) != 0 {
		t.Fatalf("a group with a surviving member was released: %v", released)
	}
}

// Whether a pid names a live non zombie process
func pidAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(b))
	return len(fields) < 3 || fields[2] != "Z"
}
