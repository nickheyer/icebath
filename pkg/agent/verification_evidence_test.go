package agent

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temp workspace with verify on stop on and the home under it
func verifyLedgerWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
	return dir
}

// Node project with test lint and dev scripts, a pnpm lockfile and a shell test runner
func verifyNodeProject(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "vitest", "lint": "eslint .", "dev": "vite"}}`)
	writeFile(t, filepath.Join(root, "pnpm-lock.yaml"), "")
	writeFile(t, filepath.Join(root, "scripts", "run_tests.sh"), "#!/bin/sh\n")
}

// Python project detected through its pytest config
func verifyPythonProject(t *testing.T, root string) {
	t.Helper()
	writeFile(t, filepath.Join(root, "pyproject.toml"), "[tool.pytest.ini_options]\n")
}

// Classified evidence of a command in the workspace, failing the test on error
func classifyIn(t *testing.T, command, cwd string, exitCode int, output string, pipelineStatus []int) *VerificationEvidence {
	t.Helper()
	ev, err := classifyVerificationCommand(command, cwd, "s1", exitCode, output, pipelineStatus)
	if err != nil {
		t.Fatalf("classify %q: %v", command, err)
	}
	return ev
}

// Ledger status of a session workspace, failing the test on error
func ledgerStatus(t *testing.T, sessionID, cwd string) map[string]any {
	t.Helper()
	status, err := verificationStatus(context.Background(), nil, sessionID, cwd)
	if err != nil {
		t.Fatalf("verification status: %v", err)
	}
	return status
}

// Recorded terminal result, failing the test on error
func recordTerminal(t *testing.T, command, cwd, sessionID string, exitCode int, output string) map[string]any {
	t.Helper()
	ev, err := recordTerminalResult(context.Background(), nil, command, cwd, sessionID, exitCode, output, nil)
	if err != nil {
		t.Fatalf("record %q: %v", command, err)
	}
	return ev
}

// Marked edit of a session workspace, failing the test on error
func markEdited(t *testing.T, sessionID, cwd string, paths ...string) map[string]any {
	t.Helper()
	out, err := markWorkspaceEdited(context.Background(), nil, sessionID, cwd, paths)
	if err != nil {
		t.Fatalf("mark edited: %v", err)
	}
	return out
}

// Recorded icebath verify run, failing the test on error
func recordVerify(t *testing.T, root, sessionID string, ok bool, scope, output string) map[string]any {
	t.Helper()
	out, err := recordVerifyRun(context.Background(), nil, root, sessionID, ok, "icebath verify", scope, output)
	if err != nil {
		t.Fatalf("record verify run: %v", err)
	}
	return out
}

// Ad hoc script path under the temp dir named for this test workspace, removed after the test
func adHocTempScript(t *testing.T, prefix, workspace, body string) string {
	t.Helper()
	tempDir, err := pypath.Gettempdir()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(tempDir, prefix+filepath.Base(filepath.Dir(workspace))+filepath.Base(workspace)+".py")
	if body != "" {
		writeFile(t, script, body)
	}
	t.Cleanup(func() { os.Remove(script) })
	return script
}

func TestLintAndTypecheckAreNotReportedAsFullTests(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyNodeProject(t, root)
	lint := classifyIn(t, "pnpm run lint", root, 0, "", nil)
	test := classifyIn(t, "pnpm run test -- tests/button.test.tsx", root, 0, "", nil)
	if lint == nil || lint.Kind != "lint" || lint.Scope != "full" {
		t.Fatalf("lint = %+v", lint)
	}
	if test == nil || test.Kind != "test" || test.Scope != "targeted" {
		t.Fatalf("test = %+v", test)
	}
}

func TestShellWrappersMatchButEchoDoesNot(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyNodeProject(t, root)
	wrapped := classifyIn(t, "env CI=1 bash scripts/run_tests.sh tests/test_widget.py", root, 0, "", nil)
	echoed := classifyIn(t, "echo scripts/run_tests.sh tests/test_widget.py", root, 0, "", nil)
	if wrapped == nil || wrapped.CanonicalCommand != "scripts/run_tests.sh" || wrapped.Scope != "targeted" {
		t.Fatalf("wrapped = %+v", wrapped)
	}
	if echoed != nil {
		t.Fatalf("echoed = %+v", echoed)
	}
}

func TestPipedVerifierUsesItsOwnPipelineStatus(t *testing.T) {
	for _, tc := range []struct {
		pipelineStatus []int
		expected       string
	}{{[]int{0, 0}, "passed"}, {[]int{1, 0}, "failed"}} {
		t.Run(fmt.Sprint(tc.pipelineStatus), func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			ev := classifyIn(t, "pytest -q 2>&1 | tail -20", root, 0, "12 passed", tc.pipelineStatus)
			if ev == nil || ev.CanonicalCommand != "pytest" || ev.Status != tc.expected {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestPipedVerifierWithoutPipelineStatusIsNotEvidence(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	if ev := classifyIn(t, "pytest -q 2>&1 | tail -20", root, 0, "", nil); ev != nil {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestPipedVerifierBehindAndCountsOnlyWhenTheChainRan(t *testing.T) {
	for _, tc := range []struct {
		command        string
		exitCode       int
		pipelineStatus []int
		expected       string
	}{
		{"true | false && pytest -q | tail -5", 1, []int{0, 1}, ""},
		{"cd . && pytest -q | tail -5", 0, []int{1, 0}, "failed"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			ev := classifyIn(t, tc.command, root, tc.exitCode, "", tc.pipelineStatus)
			got := ""
			if ev != nil {
				got = ev.Status
			}
			if got != tc.expected {
				t.Fatalf("status = %q, want %q", got, tc.expected)
			}
		})
	}
}

func TestPipedVerifierWithSinglePipelineStatusIsNotEvidence(t *testing.T) {
	for _, ps := range [][]int{{0}, {1}} {
		t.Run(fmt.Sprint(ps), func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, "pytest -q 2>&1 | tail -20", root, 0, "", ps); ev != nil {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestVenvInterpreterPathMatchesDetectedPytest(t *testing.T) {
	for _, command := range []string{
		"./venv/bin/python -m pytest tests/test_widget.py -q",
		"venv/bin/python3 -m pytest tests/test_widget.py -q",
		"/opt/venv/bin/python3.12 -m pytest tests/test_widget.py -q",
		".venv/bin/python -I -B -m pytest -o pythonpath=.",
		"/usr/bin/python3.12 -u -m pytest tests/test_widget.py -q",
		"python -W ignore -X dev -m pytest tests/test_widget.py -q",
		"py -3.11 -m pytest tests/test_widget.py -q",
	} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			ev := classifyIn(t, command, root, 0, "12 passed", nil)
			if ev == nil || ev.CanonicalCommand != "pytest" || ev.Status != "passed" {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestInterpreterFlagThatSkipsTheModuleIsNotEvidence(t *testing.T) {
	for _, command := range []string{"python --version -m pytest", "python -c pass -m pytest", "python -h -m pytest"} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, command, root, 0, "", nil); ev != nil {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestDotSlashScriptInvocationMatchesDetectedScript(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyNodeProject(t, root)
	ev := classifyIn(t, "./scripts/run_tests.sh", root, 0, "", nil)
	if ev == nil || ev.CanonicalCommand != "scripts/run_tests.sh" {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestFlagThatSkipsTheScriptIsNotEvidence(t *testing.T) {
	for _, command := range []string{
		"bash -s scripts/run_tests.sh", "sh -s scripts/run_tests.sh",
		"nice --help scripts/run_tests.sh", "timeout --help 300 scripts/run_tests.sh",
		"nohup --version scripts/run_tests.sh", "stdbuf --help scripts/run_tests.sh",
	} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyNodeProject(t, root)
			if ev := classifyIn(t, command, root, 0, "", nil); ev != nil {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestStatusPreservingWrapperStillAttributesTheRun(t *testing.T) {
	for _, command := range []string{"timeout 300 pytest -q", "nice -n 5 pytest -q", "timeout 5m pytest -q",
		"timeout -s KILL 300 pytest -q", "timeout --signal=KILL -k 10 5m pytest -q", "stdbuf -oL nohup pytest -q"} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			ev := classifyIn(t, command, root, 0, "", nil)
			if ev == nil || ev.CanonicalCommand != "pytest" || ev.Status != "passed" {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestMaskingShellControlIsNotVerificationEvidence(t *testing.T) {
	for _, command := range []string{"pytest || true", "pytest ; true", "pytest | tee test.log", "pytest &"} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, command, root, 0, "", nil); ev != nil {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestSuccessfulAndChainPreservesPassingEvidence(t *testing.T) {
	for _, command := range []string{"prepare && pytest", "pytest && report"} {
		t.Run(command, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, command, root, 0, "", nil); ev == nil || ev.Status != "passed" {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestFinalVerifierAfterSequenceOwnsShellExitStatus(t *testing.T) {
	for _, tc := range []struct {
		exitCode int
		expected string
	}{{0, "passed"}, {1, "failed"}} {
		t.Run(tc.expected, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, "prepare; pytest", root, tc.exitCode, "", nil); ev == nil || ev.Status != tc.expected {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestQuotedShellOperatorRemainsAVerifierArgument(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	if ev := classifyIn(t, "pytest -k 'passes || fails'", root, 0, "", nil); ev == nil || ev.Status != "passed" {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestShellRedirectionDoesNotHideSimpleVerifier(t *testing.T) {
	for _, redirect := range []string{"2>&1", "&> test.log"} {
		t.Run(redirect, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			verifyPythonProject(t, root)
			if ev := classifyIn(t, "pytest "+redirect, root, 0, "", nil); ev == nil || ev.Status != "passed" {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestMaskedAdHocScriptIsNotVerificationEvidence(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	script := adHocTempScript(t, "icebath-ad-hoc-", root, "raise SystemExit(1)\n")
	if ev := classifyIn(t, "python "+script+" || true", root, 0, "", nil); ev != nil {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestMaskedVerifierDoesNotClearEditedLedgerState(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	recordTerminal(t, "pytest", root, "s1", 0, "passed")
	markEdited(t, "s1", root, filepath.Join(root, "changed.py"))
	if result := recordTerminal(t, "pytest || true", root, "s1", 0, "1 failed"); result != nil {
		t.Fatalf("result = %v", result)
	}
	if got := ledgerStatus(t, "s1", root)["status"]; got != "stale" {
		t.Fatalf("status = %v", got)
	}
}

func TestTempScriptRecordsAdHocEvidenceWithoutCanonicalSuite(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	script := adHocTempScript(t, "icebath-ad-hoc-", root, "print('ok')\n")
	ev := classifyIn(t, "python "+script, root, 0, "ok", nil)
	if ev == nil || ev.CanonicalCommand != "ad-hoc verification script" || ev.Kind != "ad_hoc" || ev.Scope != "targeted" || ev.Status != "passed" {
		t.Fatalf("evidence = %+v", ev)
	}
}

func TestVersionedOrAbsoluteInterpreterRecordsAdHocEvidence(t *testing.T) {
	for _, invocation := range []string{"python3.12 %s", "/usr/bin/python3 %s", "/usr/bin/python3.12 %s", "python3 -u %s", "/usr/bin/env python3 %s"} {
		t.Run(invocation, func(t *testing.T) {
			root := verifyLedgerWorkspace(t)
			writeFile(t, filepath.Join(root, "package.json"), "{}")
			script := adHocTempScript(t, "icebath-verify-", root, "print('ok')\n")
			ev := classifyIn(t, fmt.Sprintf(invocation, script), root, 0, "ok", nil)
			if ev == nil || ev.Kind != "ad_hoc" || ev.Status != "passed" {
				t.Fatalf("evidence = %+v", ev)
			}
		})
	}
}

func TestNonInterpreterCommandTouchingTheTempScriptIsNotEvidence(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	script := adHocTempScript(t, "icebath-verify-", root, "")
	for _, command := range []string{"rm -f " + script, "/usr/bin/chmod +x " + script, "/usr/bin/cat " + script} {
		if ev := classifyIn(t, command, root, 0, "", nil); ev != nil {
			t.Fatalf("%q must not be recorded as verification evidence: %+v", command, ev)
		}
	}
}

func TestFileToolStalesEvidenceBySessionIdForAbsoluteEdit(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyNodeProject(t, root)
	target := filepath.Join(root, "src", "app.ts")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	recordTerminal(t, "pnpm test", root, "conversation", 0, "green")
	h := os.Getenv("ICEBATH_HOME")
	env := &tools.Env{Home: h, Config: config.Defaults(), Secrets: config.NewSecrets(h), Values: &sync.Map{}, Cwd: root}
	raw := tools.WriteFileTool(context.Background(), env, target, "export const ok = true\n", "turn", false, "conversation")
	decoded, err := pyjson.DecodeWTF8([]byte(raw))
	result, _ := decoded.(map[string]any)
	if err != nil || result == nil {
		t.Fatalf("write result %q: %v", raw, err)
	}
	resolved, err := pypath.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(result["files_modified"]); got != fmt.Sprint([]any{resolved}) {
		t.Fatalf("files_modified = %v in %v", got, result)
	}
	if got := ledgerStatus(t, "conversation", root)["status"]; got != "stale" {
		t.Fatalf("conversation status = %v", got)
	}
	if got := ledgerStatus(t, "turn", root)["status"]; got != "unverified" {
		t.Fatalf("turn status = %v", got)
	}
}

func TestWorkspaceVerifyClearsStaleOnOtherSession(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	markEdited(t, "s-edit", root, filepath.Join(root, "changed.py"))
	if got := ledgerStatus(t, "s-edit", root)["status"]; got != "unverified" {
		t.Fatalf("status = %v", got)
	}
	if event := recordVerify(t, root, "default", true, "full", "all green"); event == nil {
		t.Fatal("no event recorded")
	}
	status := ledgerStatus(t, "s-edit", root)
	evidence, _ := status["evidence"].(map[string]any)
	if status["status"] != "passed" || evidence["canonical_command"] != "icebath verify" {
		t.Fatalf("status = %v", status)
	}
}

func TestTargetedPartialVerifyDoesNotClearOtherSession(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	recordVerify(t, root, "s-edit", true, "full", "")
	markEdited(t, "s-edit", root, filepath.Join(root, "a.py"))
	if got := ledgerStatus(t, "s-edit", root)["status"]; got != "stale" {
		t.Fatalf("status = %v", got)
	}
	recordVerify(t, root, "", true, "targeted", "")
	if got := ledgerStatus(t, "s-edit", root)["status"]; got != "stale" {
		t.Fatalf("status after targeted = %v", got)
	}
}

func TestFailedVerifyDoesNotEraseOtherSessionEditRecord(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	markEdited(t, "s-edit", root, filepath.Join(root, "a.py"))
	recordVerify(t, root, "", false, "full", "")
	conn, err := state.ConnectSQLite(filepath.Join(os.Getenv("ICEBATH_HOME"), "verification_evidence.db"), state.SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var lastEditAt, changed sql.NullString
	if err := conn.QueryRowContext(context.Background(), "SELECT last_edit_at, changed_paths_json FROM verification_state WHERE session_id='s-edit'").Scan(&lastEditAt, &changed); err != nil {
		t.Fatalf("row: %v", err)
	}
	if !lastEditAt.Valid {
		t.Fatal("last_edit_at erased")
	}
	if got := ledgerStatus(t, "s-edit", root)["status"]; got == "passed" {
		t.Fatalf("status = %v", got)
	}
}

func TestOlderWorkspaceVerifyDoesNotClearLaterEdit(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	recordVerify(t, root, "default", true, "full", "old pass")
	markEdited(t, "s-edit", root, filepath.Join(root, "changed.py"))
	if got := ledgerStatus(t, "s-edit", root)["status"]; got != "unverified" {
		t.Fatalf("status = %v", got)
	}
}

func TestRecordingExpiresOldEditOnlyState(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyNodeProject(t, root)
	markEdited(t, "old-session", root, filepath.Join(root, "src", "app.ts"))
	cutoff := pydatetime.ISOFormatUTC(time.Now().UTC().Add(-31 * 24 * time.Hour))
	conn, err := state.ConnectSQLite(filepath.Join(os.Getenv("ICEBATH_HOME"), "verification_evidence.db"), state.SQLiteConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "UPDATE verification_state SET last_edit_at = ?", cutoff); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	recordTerminal(t, "pnpm test", root, "new-session", 0, "new green")
	status := ledgerStatus(t, "old-session", root)
	if status["status"] != "unverified" || fmt.Sprint(status["changed_paths"]) != "[]" {
		t.Fatalf("status = %v", status)
	}
}

// Swaps the temp script predicate for the test
func pinTempScriptPath(t *testing.T, fn func(token, root string) (bool, error)) {
	t.Helper()
	prev := isTempScriptPath
	isTempScriptPath = fn
	t.Cleanup(func() { isTempScriptPath = prev })
}

func TestWindowsExeInterpreterRecordsAdHocEvidence(t *testing.T) {
	for _, interpreter := range []string{`C:\Users\me\venv\Scripts\python.exe`, "python.EXE", "py -3"} {
		t.Run(interpreter, func(t *testing.T) {
			pinTempScriptPath(t, func(token, root string) (bool, error) {
				return strings.Contains(token, "icebath-verify-") && strings.HasSuffix(token, ".py"), nil
			})
			winScript := `C:\Users\me\AppData\Local\Temp\icebath-verify-x.py`
			trailing, ok, err := findAdHocMatch(interpreter+" "+winScript, t.TempDir(), 0)
			if err != nil || !ok || len(trailing) != 0 {
				t.Fatalf("match = %v %v %v", trailing, ok, err)
			}
		})
	}
}

func TestWindowsBackslashAdHocScriptPathIsMatched(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	writeFile(t, filepath.Join(root, "package.json"), "{}")
	pinTempScriptPath(t, func(token, root string) (bool, error) {
		return strings.Contains(token, "icebath-ad-hoc") && strings.Contains(token, ".py"), nil
	})
	winScript := `C:\Users\test\AppData\Local\Temp\icebath-ad-hoc-check.py`
	if _, ok, err := findAdHocMatch("python "+winScript, root, 0); err != nil || !ok {
		t.Fatalf("Windows backslash path should be matched via posix=False fallback: %v %v", ok, err)
	}
}

// Project root with a vitest package for the disabled guard tests
func disabledGuardProject(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(dir, ".icebath"))
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "")
	os.Unsetenv("ICEBATH_VERIFY_ON_STOP")
	root := filepath.Join(dir, "project")
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts": {"test": "vitest"}}`)
	return dir, root
}

func TestDisabledGuardNeverTouchesTheLedger(t *testing.T) {
	dir, root := disabledGuardProject(t)
	db := filepath.Join(dir, ".icebath", "verification_evidence.db")
	if ev := recordTerminal(t, "npm test", root, "s1", 0, ""); ev != nil {
		t.Fatalf("recorded %v", ev)
	}
	if out := markEdited(t, "s1", root, filepath.Join(root, "app.ts")); out != nil {
		t.Fatalf("marked %v", out)
	}
	if out := recordVerify(t, root, "s1", true, "full", ""); out != nil {
		t.Fatalf("verify run %v", out)
	}
	if got := ledgerStatus(t, "s1", root)["status"]; got != "disabled" {
		t.Fatalf("status = %v", got)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("ledger created: %v", err)
	}
}

func TestEnabledGuardRecordsIntoTheLedger(t *testing.T) {
	dir, root := disabledGuardProject(t)
	t.Setenv("ICEBATH_VERIFY_ON_STOP", "1")
	if ev := recordTerminal(t, "npm test", root, "s1", 0, ""); len(ev) == 0 {
		t.Fatal("nothing recorded")
	}
	if got := ledgerStatus(t, "s1", root)["status"]; got != "passed" {
		t.Fatalf("status = %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".icebath", "verification_evidence.db")); err != nil {
		t.Fatal(err)
	}
}

// Connection that records its close and can fail schema statements
type trackingLedgerConn struct {
	state.SQLiteConn
	closed     *[]state.SQLiteConn
	failSchema bool
}

func (c *trackingLedgerConn) Close() error {
	*c.closed = append(*c.closed, c.SQLiteConn)
	return c.SQLiteConn.Close()
}

func (c *trackingLedgerConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if c.failSchema && strings.Contains(query, "CREATE TABLE") {
		return nil, &state.SQLiteError{Class: state.SQLiteOperationalError, Msg: "simulated schema init failure"}
	}
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func (c *trackingLedgerConn) Raw(f func(driverConn any) error) error {
	return c.SQLiteConn.(interface {
		Raw(f func(driverConn any) error) error
	}).Raw(f)
}

// Routes ledger connections through a tracker recording every open and close
func trackLedgerConnections(t *testing.T, failSchema bool) (opened, closed *[]state.SQLiteConn) {
	t.Helper()
	opened, closed = &[]state.SQLiteConn{}, &[]state.SQLiteConn{}
	prev := verifyLedgerOpener
	verifyLedgerOpener = func(database string, cfg state.SQLiteConfig) (state.SQLiteConn, error) {
		conn, err := state.ConnectSQLite(database, cfg)
		if err != nil {
			return nil, err
		}
		*opened = append(*opened, conn)
		return &trackingLedgerConn{SQLiteConn: conn, closed: closed, failSchema: failSchema}, nil
	}
	t.Cleanup(func() { verifyLedgerOpener = prev })
	return opened, closed
}

// Whether two connection lists hold the same connections
func sameConnections(a, b []state.SQLiteConn) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		found := false
		for _, y := range b {
			found = found || x == y
		}
		if !found {
			return false
		}
	}
	return true
}

func TestLedgerOperationsCloseEveryConnection(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	opened, closed := trackLedgerConnections(t, false)
	recordTerminal(t, "python -m pytest tests/test_calc.py::test_even -q", root, "s1", 0, "1 passed")
	ledgerStatus(t, "s1", root)
	markEdited(t, "s1", root, "mod.py")
	if len(*opened) == 0 {
		t.Fatal("expected at least one connection to be opened")
	}
	if !sameConnections(*opened, *closed) {
		t.Fatalf("opened %d closed %d", len(*opened), len(*closed))
	}
}

func TestExceptionDuringOperationStillClosesConnection(t *testing.T) {
	verifyLedgerWorkspace(t)
	opened, closed := trackLedgerConnections(t, false)
	err := verifyLedgerTransaction(context.Background(), func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(context.Background(), "INSERT INTO verification_events (id) VALUES (1)")
		return err
	})
	if class, ok := state.SQLiteClassOf(err); !ok || class != state.SQLiteIntegrityError {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || len(*closed) != 1 {
		t.Fatalf("opened %d closed %d", len(*opened), len(*closed))
	}
}

func TestSchemaInitFailureStillClosesConnection(t *testing.T) {
	verifyLedgerWorkspace(t)
	opened, closed := trackLedgerConnections(t, true)
	err := verifyLedgerTransaction(context.Background(), func(state.SQLiteConn) error { return nil })
	if class, ok := state.SQLiteClassOf(err); !ok || class != state.SQLiteOperationalError {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 1 || len(*closed) != 1 {
		t.Fatalf("opened %d closed %d", len(*opened), len(*closed))
	}
}

func TestEvidenceRecorderHookRecordsVerifyRun(t *testing.T) {
	root := verifyLedgerWorkspace(t)
	verifyPythonProject(t, root)
	event, err := verify.EvidenceRecorder(context.Background(), root, "s1", true, "icebath verify", "full", "ok")
	if err != nil || event == nil || event["kind"] != "verify" {
		t.Fatalf("event = %v, %v", event, err)
	}
	if got := ledgerStatus(t, "s1", root)["status"]; got != "passed" {
		t.Fatalf("status = %v", got)
	}
}
