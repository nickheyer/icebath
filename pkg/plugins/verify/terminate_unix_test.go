//go:build !windows

package verify

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func startSleeper(t *testing.T, ownGroup bool) *childProcess {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := newChildProcess(cmd)
	t.Cleanup(func() {
		if !proc.poll() {
			_ = cmd.Process.Kill()
			proc.wait(5 * time.Second)
		}
	})
	return proc
}

func TestRealSharedGroupChildDoesNotSignalUs(t *testing.T) {
	proc := startSleeper(t, false)
	ours, _ := syscall.Getpgid(0)
	if g, _ := syscall.Getpgid(proc.pid()); g != ours {
		t.Fatalf("child group %d, ours %d", g, ours)
	}
	if err := terminateProcessGroup(proc); err != nil {
		t.Fatal(err)
	}
	if !proc.wait(5 * time.Second) {
		t.Fatal("shared group child survived")
	}
}

func TestRealGroupLeaderChildIsGroupKilled(t *testing.T) {
	proc := startSleeper(t, true)
	if g, _ := syscall.Getpgid(proc.pid()); g != proc.pid() {
		t.Fatalf("child group %d, pid %d", g, proc.pid())
	}
	if err := terminateProcessGroup(proc); err != nil {
		t.Fatal(err)
	}
	if !proc.wait(5 * time.Second) {
		t.Fatal("group leader child survived")
	}
}
