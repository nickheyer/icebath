package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashboardTuiFlagIsAcceptedNotRejected(t *testing.T) {
	tmp := t.TempDir()
	h, user := filepath.Join(tmp, "home"), filepath.Join(tmp, "user")
	for _, dir := range []string{h, user} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cli := exec.Command(os.Args[0], "dashboard", "--no-open", "--tui", "--host", "127.0.0.1", "--port", "39997", "--status")
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name != "ICEBATH_HOME" && name != "HOME" {
			cli.Env = append(cli.Env, kv)
		}
	}
	cli.Env = append(cli.Env, "ICEBATH_HOME="+h, "HOME="+user, runMainEnv+"=1")
	var stdout, stderr bytes.Buffer
	cli.Stdout, cli.Stderr = &stdout, &stderr
	err := cli.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "unrecognized arguments") || strings.Contains(combined, "unknown flag") {
		t.Fatalf("the flag was rejected:\n%s", combined)
	}
	if strings.Contains(stderr.String(), "--tui") {
		t.Fatalf("stderr names the flag:\n%s", stderr.String())
	}
	if exit != nil && exit.ExitCode() == 2 {
		t.Fatalf("usage error exit:\n%s", combined)
	}
}
