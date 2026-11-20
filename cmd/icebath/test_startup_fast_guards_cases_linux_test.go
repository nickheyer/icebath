package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCliStartsFromADeletedCwd(t *testing.T) {
	h := fastVersionHome(t)
	gone := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `cd "$1" && rmdir "$1" && exec "$0" --version`, os.Args[0], gone)
	cmd.Env = fastVersionEnv(t, []string{"ICEBATH_HOME=" + h, "TERMUX_VERSION="})
	stdout, stderr, code := runCaptured(t, cmd)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the cwd was not removed before the start, stat %v", err)
	}
	if !strings.Contains(stdout, "Icebath Agent v") {
		t.Fatalf("stdout\n%s", stdout)
	}
	if strings.Contains(stderr, "FileNotFoundError") || strings.Contains(stderr, "no such file or directory") || strings.Contains(stderr, "panic:") {
		t.Fatalf("stderr\n%s", stderr)
	}
}
