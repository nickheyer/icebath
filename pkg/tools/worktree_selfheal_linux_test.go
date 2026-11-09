//go:build linux

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRepackStampedeTimeoutKillsTheWholeRepackTree(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "home"))
	shimDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(tmp, "grandchild.pid")
	shim := filepath.Join(shimDir, "git")
	if err := os.WriteFile(shim, []byte(fmt.Sprintf("#!/bin/sh\nsleep 300 &\necho $! > %s\nwait\n", pidfile)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := repackTimeout
	t.Cleanup(func() { repackTimeout = prev })
	repackTimeout = 1
	runBoundedRepack(tmp)
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", grandchild)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = exec.Command("kill", "-9", strconv.Itoa(grandchild)).Run()
	t.Fatal("pack-objects stand-in survived the repack timeout")
}
