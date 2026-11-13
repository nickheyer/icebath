package gateway

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRouteScriptsRunOnThePythonFoundOnPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture interpreter is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "python"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if got, err := webhookSysExecutable(); err != nil || got != filepath.Join(dir, "python") {
		t.Fatalf("interpreter %q err %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := webhookSysExecutable(); err != nil || got != filepath.Join(dir, "python3") {
		t.Fatalf("python3 not preferred: %q err %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := webhookSysExecutable(); err == nil || !strings.Contains(err.Error(), "need a Python interpreter on PATH") {
		t.Fatalf("an empty PATH resolved: %v", err)
	}
}
