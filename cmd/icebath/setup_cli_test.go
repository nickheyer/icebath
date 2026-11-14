package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/cliui"
)

func TestArgparseReconfigureFlagReachesCmdSetup(t *testing.T) {
	var captured map[string]any
	prev := runSetupWizard
	runSetupWizard = func(_ context.Context, ns map[string]any) (int, error) {
		if captured == nil {
			captured = ns
		}
		return 0, nil
	}
	t.Cleanup(func() { runSetupWizard = prev })
	if err := newCLI().Execute([]string{"setup", "--reconfigure"}); err != nil {
		t.Fatal(err)
	}
	if captured["reconfigure"] != true || captured["quick"] != false {
		t.Fatalf("namespace %v", captured)
	}
}

// Child process environment with an empty home and no credential the provider probe could find
func firstRunChildEnv(t *testing.T, mode string) []string {
	t.Helper()
	tmp := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		key := strings.SplitN(kv, "=", 2)[0]
		if strings.HasSuffix(key, "_API_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_BASE_URL") ||
			strings.HasPrefix(key, "ICEBATH_") || strings.HasPrefix(key, "AWS_") || key == "HOME" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HOME="+tmp, "ICEBATH_HOME="+tmp+"/.icebath", "ICEBATH_TEST_FIRST_RUN_CHILD="+mode)
}

// Child half of the first run tests, a bare chat with no provider configured
func TestFirstRunSetupGuardChild(t *testing.T) {
	switch os.Getenv("ICEBATH_TEST_FIRST_RUN_CHILD") {
	case "headless":
		exit(cmdChat(&chatArgs{}))
	case "tty-eof":
		app.SetStreams(&cliui.Console{In: os.Stdin, Out: os.Stdout, StdinTTY: func() bool { return true }}, os.Stderr)
		firstRunSetupGuard(&chatArgs{})
		exit(0)
	}
}

// Runs the first run child in a mode with stdin closed, its stdout and exit code
func runFirstRunChild(t *testing.T, mode string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestFirstRunSetupGuardChild$")
	cmd.Env = firstRunChildEnv(t, mode)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatal(err)
	}
	return string(out), cmd.ProcessState.ExitCode()
}

func TestNonInteractiveSetupChatFirstRunHeadlessSkipsSetupPrompt(t *testing.T) {
	out, code := runFirstRunChild(t, "headless")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "icebath config set model.provider custom") || strings.Contains(out, "Run setup now?") || strings.Contains(out, "Setup Wizard") {
		t.Fatalf("screen\n%s", out)
	}
}

func TestFirstRunSetupGuardReadsEOFAsNo(t *testing.T) {
	out, code := runFirstRunChild(t, "tty-eof")
	want := "\nIt looks like Icebath isn't configured yet -- no API keys or providers found.\n\n  Run:  icebath setup\n\n" +
		"Run setup now? [Y/n] \nYou can run 'icebath setup' at any time to configure.\n"
	if code != 1 || !strings.HasSuffix(out, want) {
		t.Fatalf("exit %d screen %q", code, out)
	}
}
