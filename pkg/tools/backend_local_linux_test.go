//go:build linux

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestTimeoutKillReapsSetsidGrandchild(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	l := NewLocalEnvironment(context.Background(), newTestToolEnv(t), dir, 15, nil, true)
	defer l.Cleanup(false)
	pidFile := filepath.Join(dir, "grandchild.pid")
	cmd := "setsid bash -c 'echo $$ > " + shQuote(pidFile) + "; exec sleep 60' & sleep 30"
	res, err := l.Execute(context.Background(), cmd, ExecOptions{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.ReturnCode != 124 {
		t.Fatalf("%+v", res)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := 0
	for _, c := range strings.TrimSpace(string(data)) {
		pid = pid*10 + int(c-'0')
	}
	waitUntil(t, "the setsid grandchild to be reaped", func() bool {
		return syscall.Kill(pid, 0) != nil
	})
}

func TestKillProcessNeverKillpgsTheCallersOwnGroup(t *testing.T) {
	bash := requireBash(t)
	p, err := popenBash([]string{bash, "-c", "sleep 30"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pgid, _ := syscall.Getpgid(p.Pid()); pgid != syscall.Getpgrp() {
		t.Fatal("child must share our group for this test")
	}
	if err := killLocalProcessTree(p); err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(5); err != nil || code == 0 {
		t.Fatalf("child not killed: %d %v", code, err)
	}
}

func TestKillProcessUsesCachedPgidIfWrapperAlreadyExited(t *testing.T) {
	bash := requireBash(t)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")
	p, err := popenBash([]string{bash, "-c", "bash -c 'echo $$ > " + shQuote(pidFile) + "; exec sleep 60' & exit 0"}, nil, func(c *exec.Cmd) { startNewSession(c) })
	if err != nil {
		t.Fatal(err)
	}
	p.pgid = p.Pid()
	if start, ok := processStartTime(p.Pid()); ok {
		p.pgidStart = strconv.FormatInt(start, 10)
	}
	p.Wait(5)
	waitUntil(t, "the orphan pid file", func() bool { _, err := os.Stat(pidFile); return err == nil })
	data, _ := os.ReadFile(pidFile)
	orphan := 0
	for _, c := range strings.TrimSpace(string(data)) {
		orphan = orphan*10 + int(c-'0')
	}
	if err := killLocalProcessTree(p); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the orphaned group member to die", func() bool { return syscall.Kill(orphan, 0) != nil })
}
