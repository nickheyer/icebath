package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNodeToolRunnableRejectsEmptyAndProbesVersion(t *testing.T) {
	if NodeToolRunnable("") {
		t.Fatal("empty path reported runnable")
	}
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
