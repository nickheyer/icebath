package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Proxies that refuse at once so the update check of --version answers offline
var offlineVersionEnv = []string{
	"HTTPS_PROXY=http://127.0.0.1:9", "HTTP_PROXY=http://127.0.0.1:9",
	"https_proxy=http://127.0.0.1:9", "http_proxy=http://127.0.0.1:9",
	"NO_PROXY=", "no_proxy=",
}

// Environment of a main child with the offline proxies, the overrides, a scratch user home and no dev flag
func fastVersionEnv(t *testing.T, overrides []string) []string {
	t.Helper()
	set := append(append([]string{"HOME=" + t.TempDir()}, offlineVersionEnv...), overrides...)
	replaced := map[string]bool{"ICEBATH_DEV": true}
	for _, kv := range set {
		name, _, _ := strings.Cut(kv, "=")
		replaced[name] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !replaced[name] {
			env = append(env, kv)
		}
	}
	return append(append(env, set...), runMainEnv+"=1")
}

// Output and exit status of the command run to completion
func runCaptured(t *testing.T, cmd *exec.Cmd) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

// Runs icebath --version as a child of the test binary with the overrides
func runFastVersion(t *testing.T, overrides ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "--version")
	cmd.Env = fastVersionEnv(t, overrides)
	return runCaptured(t, cmd)
}

// Fresh empty home directory named like the default home
func fastVersionHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestFastVersionParityOnTermux(t *testing.T) {
	stdout, stderr, code := runFastVersion(t, "ICEBATH_HOME="+fastVersionHome(t), "TERMUX_VERSION=0.118")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Icebath Agent v") {
		t.Fatalf("stdout\n%s", stdout)
	}
	if strings.Contains(stderr, "panic:") {
		t.Fatalf("stderr\n%s", stderr)
	}
}

func TestFastVersionReportsInstallMethodStamp(t *testing.T) {
	h := fastVersionHome(t)
	if err := os.WriteFile(filepath.Join(h, ".install_method"), []byte("git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runFastVersion(t, "ICEBATH_HOME="+h)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Install method: git") {
		t.Fatalf("stdout\n%s", stdout)
	}
}

func TestFastVersionParity(t *testing.T) {
	stdout, stderr, code := runFastVersion(t, "ICEBATH_HOME="+fastVersionHome(t))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	for _, field := range []string{"Icebath Agent v", "Install directory:", "\nGo: " + hostinfo.GoVersion() + "\n"} {
		if !strings.Contains(stdout, field) {
			t.Fatalf("fast --version output missing %q:\n%s", field, stdout)
		}
	}
	for _, field := range []string{"Python:", "OpenAI SDK:"} {
		if strings.Contains(stdout, field) {
			t.Fatalf("fast --version output of the Go binary states %q:\n%s", field, stdout)
		}
	}
	if strings.Contains(stderr, "panic:") {
		t.Fatalf("stderr\n%s", stderr)
	}
}
