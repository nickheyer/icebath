package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
)

// Runs one slack command line in process and returns its stdout, stderr and exit code
func slackGroupRun(t *testing.T, argv ...string) (string, string, int) {
	t.Helper()
	leftCoreCLIHome(t, argv...)
	var err error
	var stderr string
	stdout := captureStream(t, &os.Stdout, func() {
		stderr = captureStream(t, &os.Stderr, func() { err = newCLI().Execute(argv) })
	})
	var exit *commands.ExitError
	switch {
	case err == nil:
		return stdout, stderr, 0
	case errors.As(err, &exit):
		return stdout, stderr, exit.Code
	}
	t.Fatalf("%v: %v", argv, err)
	return "", "", 0
}

func TestSlackManifestConsoleExitStatusTooShortLongDescriptionExitsTwo(t *testing.T) {
	stdout, stderr, code := slackGroupRun(t, "slack", "manifest", "--long-description", strings.Repeat("x", 174))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "at least 175 characters") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestSlackManifestConsoleExitStatusMissingLongDescriptionFileExitsTwo(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.md")
	stdout, stderr, code := slackGroupRun(t, "slack", "manifest", "--long-description-file", missing)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "cannot read long description") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
}
