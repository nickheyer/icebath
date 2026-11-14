package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestNoneAndEmptyRejected(t *testing.T) {
	if NodeToolRunnable("") {
		t.Fatal("empty path reported runnable")
	}
}

func TestNodeToolRunnableProbesVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	dir := t.TempDir()
	ok := filepath.Join(dir, "node")
	bad := filepath.Join(dir, "npm")
	plain := filepath.Join(dir, "npx")
	os.WriteFile(ok, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	os.WriteFile(bad, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	os.WriteFile(plain, []byte("#!/bin/sh\nexit 0\n"), 0o644)
	var probed [][]string
	saved := runVersionProbe
	t.Cleanup(func() { runVersionProbe = saved })
	runVersionProbe = func(argv []string, env map[string]string) (int, bool) {
		probed = append(probed, argv)
		return saved(argv, env)
	}
	if !NodeToolRunnable(ok) || NodeToolRunnable(bad) || NodeToolRunnable(plain) || NodeToolRunnable(filepath.Join(dir, "gone")) {
		t.Fatal("runnable verdicts wrong")
	}
	if len(probed) != 2 || probed[0][1] != "--version" {
		t.Fatalf("probes = %v, want only the two executable files probed with --version", probed)
	}
}

func TestVersionProbeWaitsForItsPipesLikeCommunicate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell")
	}
	dir := t.TempDir()
	held := filepath.Join(dir, "node")
	os.WriteFile(held, []byte("#!/bin/sh\nsleep 12 &\nexit 0\n"), 0o755)
	started := time.Now()
	if NodeToolRunnable(held) {
		t.Fatal("a probe whose output pipe stays open past the timeout reported runnable")
	}
	if elapsed := time.Since(started); elapsed < 9*time.Second || elapsed > 11*time.Second {
		t.Fatalf("probe returned after %v, want the ten second timeout", elapsed)
	}
	chatty := filepath.Join(dir, "npm")
	os.WriteFile(chatty, []byte("#!/bin/sh\necho v10.0.0\necho warn >&2\nexit 0\n"), 0o755)
	if !NodeToolRunnable(chatty) {
		t.Fatal("a probe printing its version reported not runnable")
	}
}
