//go:build !windows

package updaterecord

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const ownerWatchScript = `b=$(dd bs=1 count=1 2>/dev/null); [ -n "$b" ] || kill -s KILL -- "-$1" 2>/dev/null`

// Runs git under the parent death signal while the lock is held, or under the owner watch without one
func runGitPlatform(ctx context.Context, argv []string, timeout time.Duration, held bool) (*GitResult, error) {
	if held && deathSignalAttr() == nil {
		killGroup := func(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		return runCaptured(ctx, argv, timeout, childSpawn{
			attr:      &syscall.SysProcAttr{Setsid: true},
			started:   func(cmd *exec.Cmd) (func(), error) { return ownerWatch(cmd.Process.Pid), nil },
			killTree:  killGroup,
			interrupt: killGroup,
		})
	}
	var attr *syscall.SysProcAttr
	if held {
		attr = deathSignalAttr()
	}
	return runCaptured(ctx, argv, timeout, childSpawn{attr: attr, killTree: killTreeOf, interrupt: killChild})
}

// Starts a watcher that kills the child's process group when this process dies, nil when it cannot start
func ownerWatch(pid int) func() {
	r, w, err := os.Pipe()
	if err != nil {
		slog.Warn(fmt.Sprintf("Could not watch update child %d for its owner's death: %v", pid, err))
		return nil
	}
	watcher := exec.Command("/bin/sh", "-c", ownerWatchScript, "sh", strconv.Itoa(pid))
	watcher.Stdin = r
	watcher.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = watcher.Start()
	r.Close()
	if err != nil {
		w.Close()
		slog.Warn(fmt.Sprintf("Could not watch update child %d for its owner's death: %v", pid, err))
		return nil
	}
	return func() {
		_, _ = w.Write([]byte("x"))
		w.Close()
		done := make(chan struct{})
		go func() { _ = watcher.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
}

// Starts the owner death watcher for a child leading its own session, nil when it cannot start
func OwnerWatch(pid int) func() { return ownerWatch(pid) }

// Process attributes that kill a child with its owner thread, nil where the platform has no such signal
func DeathSignalAttr() *syscall.SysProcAttr { return deathSignalAttr() }
