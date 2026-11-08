package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Child mode env carrying the root a re-executed test binary records a failed verify run for
const verifyRunChildEnv = "ICEBATH_TEST_VERIFY_RUN_CHILD"

// Records a failed icebath verify run from this separate process when the child env is set
func dispatchVerifyRunChild() {
	root := os.Getenv(verifyRunChildEnv)
	if root == "" {
		return
	}
	if _, err := recordVerifyRun(context.Background(), nil, root, "s1", false, "icebath verify", "full", ""); err != nil {
		fmt.Println(err)
		os.Exit(2)
	}
	os.Exit(0)
}

// Node project with test and lint scripts and a pnpm lockfile
func stopNodeProject(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "vitest", "lint": "eslint ."}}`)
	writeFile(t, filepath.Join(root, "pnpm-lock.yaml"), "")
}

// Clears every env signal the verify on stop switch consults
func clearVerifyEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ICEBATH_VERIFY_ON_STOP", "ICEBATH_PLATFORM", "ICEBATH_SESSION_PLATFORM", "ICEBATH_SESSION_SOURCE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Verify on stop nudge of the session for the changed paths with default attempts
func stopNudge(t *testing.T, sessionID string, changedPaths ...string) string {
	t.Helper()
	nudge, err := buildVerifyOnStopNudge(context.Background(), nil, sessionID, changedPaths, 0, defaultVerifyOnStopMaxAttempts)
	if err != nil {
		t.Fatalf("nudge: %v", err)
	}
	return nudge
}

// Records a terminal result with its pipeline status, failing the test on error
func recordTerminalPiped(t *testing.T, command, cwd string, exitCode int, output string, pipelineStatus []int) {
	t.Helper()
	if _, err := recordTerminalResult(context.Background(), nil, command, cwd, "s1", exitCode, output, pipelineStatus); err != nil {
		t.Fatalf("record %q: %v", command, err)
	}
}

func TestVerifyOnStopEnvCanEnable(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	t.Setenv("ICEBATH_SESSION_PLATFORM", "telegram")
	if !verifyOnStopEnabled(context.Background(), nil, map[string]any{"agent": map[string]any{}}) {
		t.Fatal("env 1 must force verify on stop on")
	}
}

func TestVerifyOnStopAutoOnForInteractiveSurfaces(t *testing.T) {
	for _, source := range []string{"cli", "tui", "desktop", "codex", "local"} {
		t.Run(source, func(t *testing.T) {
			clearVerifyEnv(t)
			t.Setenv("ICEBATH_SESSION_SOURCE", source)
			if !verifyOnStopEnabled(context.Background(), nil, map[string]any{"agent": map[string]any{"verify_on_stop": "auto"}}) {
				t.Fatalf("auto must resolve on for %s", source)
			}
		})
	}
}

func TestVerifyOnStopMissingValueDefaultsOff(t *testing.T) {
	clearVerifyEnv(t)
	t.Setenv("ICEBATH_SESSION_SOURCE", "cli")
	for _, cfg := range []map[string]any{
		{"agent": map[string]any{}},
		{"agent": map[string]any{"verify_on_stop": "bogus"}},
		{},
	} {
		if verifyOnStopEnabled(context.Background(), nil, cfg) {
			t.Fatalf("config %v must resolve off", cfg)
		}
	}
}

func TestNudgeChecksAllEditedWorkspaces(t *testing.T) {
	dir := verifyLedgerWorkspace(t)
	projectA, projectB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	stopNodeProject(t, projectA)
	stopNodeProject(t, projectB)
	changedA := filepath.Join(projectA, "src", "app.ts")
	changedB := filepath.Join(projectB, "src", "app.ts")
	recordTerminal(t, "pnpm test", projectA, "s1", 0, "green")
	markEdited(t, "s1", projectB, changedB)
	nudge := stopNudge(t, "s1", changedA, changedB)
	if nudge == "" || !strings.Contains(nudge, changedB) {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNoSuiteNudgeUsesCanonicalTempDir(t *testing.T) {
	dir := verifyLedgerWorkspace(t)
	project := filepath.Join(dir, "project")
	writeFile(t, filepath.Join(project, "package.json"), "{}")
	realTemp := filepath.Join(dir, "real-temp")
	if err := os.Mkdir(realTemp, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedTemp := filepath.Join(dir, "linked-temp")
	if err := os.Symlink(realTemp, linkedTemp); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", linkedTemp)
	resolvedReal, err := filepath.EvalSymlinks(realTemp)
	if err != nil {
		t.Fatal(err)
	}
	nudge := stopNudge(t, "s1", filepath.Join(project, "src", "app.ts"))
	if nudge == "" || !strings.Contains(nudge, resolvedReal) || strings.Contains(nudge, linkedTemp) {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestAdHocPassSatisfiesNoSuiteStopLoop(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	changed := filepath.Join(root, "src", "app.ts")
	script := adHocTempScript(t, "icebath-ad-hoc-stop-", root, "print('ok')\n")
	recordTerminal(t, "python "+script, root, "s1", 0, "ok")
	os.Remove(script)
	if nudge := stopNudge(t, "s1", changed); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeAttemptsAreBounded(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	stopNodeProject(t, root)
	changed := filepath.Join(root, "src", "app.ts")
	markEdited(t, "s1", root, changed)
	nudge, err := buildVerifyOnStopNudge(context.Background(), nil, "s1", []string{changed}, 2, 2)
	if err != nil || nudge != "" {
		t.Fatalf("nudge = %q, %v", nudge, err)
	}
}

func TestPipedVerificationRunSatisfiesTheGuard(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	stopNodeProject(t, root)
	changed := filepath.Join(root, "src", "app.ts")
	markEdited(t, "s1", root, changed)
	recordTerminalPiped(t, "pnpm test 2>&1 | tail -20", root, 0, "ok", []int{0, 0})
	if nudge := stopNudge(t, "s1", changed); nudge != "" {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestNudgeExplainsOnlyACheckRunTheShellCouldNotAttribute(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	stopNodeProject(t, root)
	changed := filepath.Join(root, "src", "app.ts")
	markEdited(t, "s1", root, changed)
	nudge := func() string {
		text := stopNudge(t, "s1", changed)
		if text == "" {
			t.Fatal("no nudge")
		}
		return text
	}
	noRun := nudge()
	recordTerminalPiped(t, "pnpm test || true", root, 0, "", nil)
	hidden := nudge()
	recordTerminalPiped(t, "pnpm test 2>&1 | tail -5", root, 0, "", []int{1, 0})
	scored := nudge()
	if strings.Contains(noRun, "did not count") {
		t.Fatalf("no run nudge = %q", noRun)
	}
	if !strings.Contains(hidden, "Your last check run (`pnpm test || true`) did not count") {
		t.Fatalf("hidden nudge = %q", hidden)
	}
	if strings.Contains(scored, "did not count") {
		t.Fatalf("scored nudge = %q", scored)
	}
}

func TestNudgeDropsTheUnscoredRunWarningOnceHermesVerifyRecords(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	stopNodeProject(t, root)
	changed := filepath.Join(root, "src", "app.ts")
	markEdited(t, "s1", root, changed)
	recordTerminalPiped(t, "pnpm test || true", root, 0, "", nil)
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), verifyRunChildEnv+"="+root)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("child verify run: %v %s", err, out)
	}
	nudge := stopNudge(t, "s1", changed)
	if nudge == "" || !strings.Contains(nudge, "icebath verify") || strings.Contains(nudge, "did not count") {
		t.Fatalf("nudge = %q", nudge)
	}
}

func TestMixedDocAndCodeEditStillNudges(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	stopNodeProject(t, root)
	doc := filepath.Join(root, "README.md")
	code := filepath.Join(root, "src", "app.ts")
	markEdited(t, "s1", root, code)
	nudge := stopNudge(t, "s1", doc, code)
	if nudge == "" || !strings.Contains(nudge, code) || strings.Contains(nudge, doc) {
		t.Fatalf("nudge = %q", nudge)
	}
}
