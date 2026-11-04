package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// test_terminal_exit_semantics.py
func TestInterpretExitCode(t *testing.T) {
	for _, cmd := range []string{"grep foo bar", "diff a b", "test -f /etc/passwd"} {
		if interpretExitCode(cmd, 0) != "" {
			t.Fatalf("success note for %q", cmd)
		}
	}
	for _, cmd := range []string{"grep 'pattern' file.txt", "egrep 'pattern' file.txt", "fgrep 'pattern' file.txt", "rg 'foo' .", "ag 'foo' .", "ack 'foo' ."} {
		if note := interpretExitCode(cmd, 1); !strings.Contains(strings.ToLower(note), "no matches") {
			t.Fatalf("%q -> %q", cmd, note)
		}
	}
	if note := interpretExitCode("diff file1 file2", 1); !strings.Contains(strings.ToLower(note), "differ") {
		t.Fatal(note)
	}
	if note := interpretExitCode("test -f /nonexistent", 1); !strings.Contains(strings.ToLower(note), "false") {
		t.Fatal(note)
	}
	if interpretExitCode("FOO=bar", 1) != "" {
		t.Fatal("env only command produced a note")
	}
	if note := interpretExitCode("cat x | /usr/bin/grep foo", 1); !strings.Contains(note, "No matches") {
		t.Fatal("pipeline tail and full path not honoured")
	}
}

// test_terminal_signal_exit.py
func TestInterpretSignalExit(t *testing.T) {
	for code, expect := range map[int]string{-9: "SIGKILL", -11: "SIGSEGV", -15: "SIGTERM", 137: "SIGKILL", 139: "SIGSEGV", 143: "SIGTERM"} {
		if note := interpretSignalExit(code); !strings.Contains(note, expect) {
			t.Fatalf("%d -> %q", code, note)
		}
	}
	if note := interpretSignalExit(-31); !strings.Contains(note, "31") {
		t.Fatal(note)
	}
	if interpretSignalExit(128+40) != "" || interpretSignalExit(130) != "" || interpretSignalExit(-2) != "" {
		t.Fatal("uncurated or sigint signal produced a note")
	}
	for _, code := range []int{0, 1, 2, 42, 100, 127, 128} {
		if interpretSignalExit(code) != "" {
			t.Fatalf("normal code %d noted", code)
		}
	}
	if note := interpretExitCode("python3 crash.py", -11); !strings.Contains(note, "SIGSEGV") {
		t.Fatal(note)
	}
	if note := interpretExitCode("grep foo huge.log", 137); !strings.Contains(note, "SIGKILL") {
		t.Fatal(note)
	}
}

func finalizeTestEnv(t *testing.T) *Env {
	t.Helper()
	resetTerminalState(t)
	return newTestToolEnv(t)
}

// test_terminal_environment_recreated_notice.py
func TestFinalizerWarnsWhenBackendFlaggedRecreation(t *testing.T) {
	env := finalizeTestEnv(t)
	e := newFakeTermEnv("/root", nil)
	e.envType = "docker"
	flagged := finalizeForegroundResult(env, "echo hi", &ExecResult{Output: "hi", EnvironmentRecreated: true}, e, "docker", "t", "t", "s", "k", "", "", "")
	var r map[string]any
	json.Unmarshal([]byte(flagged), &r)
	mustContain(t, resultString(r, "environment_recreated"), "recreated")
	clean := finalizeForegroundResult(env, "echo hi", &ExecResult{Output: "hi"}, e, "docker", "t", "t", "s", "k", "", "", "")
	if strings.Contains(clean, "environment_recreated") {
		t.Fatal("clean result carries the recreated note")
	}
}

func TestExecuteFoldsRecreationFlagOnce(t *testing.T) {
	env := newTestToolEnv(t)
	local := NewLocalEnvironment(context.Background(), env, t.TempDir(), 30, nil, true)
	defer local.Cleanup(false)
	local.MarkRecreated()
	res, err := local.Execute(context.Background(), "echo hi", ExecOptions{})
	if err != nil || res.ReturnCode != 0 || !res.EnvironmentRecreated {
		t.Fatalf("first result %+v %v", res, err)
	}
	res2, _ := local.Execute(context.Background(), "echo again", ExecOptions{})
	if res2.EnvironmentRecreated {
		t.Fatal("flag not consumed")
	}
}

// test_terminal_output_transform_hook.py
func runTransformTerminal(t *testing.T, output string, returncode int, hook func(map[string]any) []any, approval GuardResult, command string) map[string]any {
	t.Helper()
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	env.Secrets = config.NewSecrets(env.Home)
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "1")
	if approval.Approved || approval.Description != "" {
		checkAllGuardsFn = func(context.Context, *Env, string, string, bool) GuardResult { return approval }
	}
	TerminalOutputTransformHook = hook
	cacheFakeEnv("default", newFakeTermEnv(dir, func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return &ExecResult{Output: output, ReturnCode: returncode}, nil
	}))
	return runTerminal(t, env, TerminalRequest{Command: command})
}

func TestTerminalOutputUnchangedWhenTransformHookNotRegistered(t *testing.T) {
	r := runTransformTerminal(t, "plain output", 0, nil, GuardResult{}, "echo hello")
	if r["output"] != "plain output" || r["exit_code"] != float64(0) || r["error"] != nil {
		t.Fatalf("%v", r)
	}
}

func TestTerminalOutputTransformStillRunsStripAndRedact(t *testing.T) {
	secret := "sk-proj-abc123def456ghi789jkl012mno345"
	r := runTransformTerminal(t, "plain output", 0, func(map[string]any) []any {
		return []any{" \x1b[31mOPENAI_API_KEY=" + secret + "\x1b[0m "}
	}, GuardResult{}, "echo hello")
	out := resultString(r, "output")
	if strings.Contains(out, "\x1b") || strings.Contains(out, secret) || !strings.Contains(out, "OPENAI_API_KEY=") || strings.Contains(out, "abc123def456") {
		t.Fatalf("output %q", out)
	}
}

func TestTerminalOutputTransformDoesNotChangeApprovalOrExitCodeMeaning(t *testing.T) {
	r := runTransformTerminal(t, "original output", 1, func(map[string]any) []any { return []any{"replaced output"} },
		GuardResult{Approved: true, UserApproved: true, Description: "dangerous command"}, "grep foo bar")
	if r["output"] != "replaced output" {
		t.Fatalf("%v", r)
	}
	if resultString(r, "approval") != "Command required approval (dangerous command) and was approved by the user." {
		t.Fatalf("approval %v", r["approval"])
	}
	if resultString(r, "exit_code_meaning") != "No matches found (not an error)" {
		t.Fatalf("meaning %v", r["exit_code_meaning"])
	}
}

func TestLargeProcessOutputIsBoundedBeforeSudoAndPluginHooks(t *testing.T) {
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	env.Config = config.Tree{"tool_output": map[string]any{"max_bytes": 10000}}
	ResetToolOutputLimitsCache()
	defer ResetToolOutputLimitsCache()
	var hookInputs []string
	TerminalOutputTransformHook = func(payload map[string]any) []any {
		hookInputs = append(hookInputs, payload["output"].(string))
		return nil
	}
	local := NewLocalEnvironment(context.Background(), env, dir, 10, nil, true)
	defer local.Cleanup(false)
	cacheFakeEnv("default", local)
	command := "python3 -c \"import sys; sys.stdout.write('HEAD-SENTINEL\\n' + 'x' * 2000000 + '\\nTAIL-SENTINEL')\""
	r := runTerminal(t, env, TerminalRequest{Command: command})
	if len(hookInputs) != 1 || len(hookInputs[0]) > 10000 || !strings.HasPrefix(hookInputs[0], "HEAD-SENTINEL") || !strings.HasSuffix(hookInputs[0], "TAIL-SENTINEL") || !strings.Contains(hookInputs[0], "[OUTPUT TRUNCATED") {
		t.Fatalf("hook input %d %q", len(hookInputs), truncateForLog(hookInputs))
	}
	if len(resultString(r, "output")) > 10000 {
		t.Fatal("result output over the limit")
	}
}

func truncateForLog(in []string) string {
	if len(in) == 0 {
		return ""
	}
	s := in[0]
	if len(s) > 200 {
		return s[:100] + "..." + s[len(s)-100:]
	}
	return s
}

// test_terminal_truncation_spill.py
func spillEnv(t *testing.T) (*Env, string) {
	t.Helper()
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	env.Config = config.Tree{"tool_output": map[string]any{"max_bytes": 2000, "max_lines": 2000, "max_line_length": 2000}}
	ResetToolOutputLimitsCache()
	t.Cleanup(ResetToolOutputLimitsCache)
	local := NewLocalEnvironment(context.Background(), env, dir, 30, nil, true)
	t.Cleanup(func() { local.Cleanup(false) })
	cacheFakeEnv("default", local)
	return env, dir
}

func TestTruncatedOutputHasMetadataAndSpill(t *testing.T) {
	env, _ := spillEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "python3 -c \"print('marker_head'); [print(f'row_{i}', 'x'*80) for i in range(200)]; print('marker_tail')\"", TaskID: "t-spill-1"})
	if r["exit_code"] != float64(0) {
		t.Fatalf("%v", r)
	}
	mustContain(t, resultString(r, "output"), "OUTPUT TRUNCATED")
	if r["output_total_chars"].(float64) <= 2000 {
		t.Fatal("total chars")
	}
	full, err := os.ReadFile(resultString(r, "full_output_path"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"marker_head", "marker_tail", "row_100 "} {
		mustContain(t, string(full), s)
	}
	mustContain(t, resultString(r, "truncation_note"), "read_file")
}

func TestSmallOutputHasNoMetadata(t *testing.T) {
	env, _ := spillEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "echo tiny", TaskID: "t-spill-2"})
	if r["exit_code"] != float64(0) || resultHas(r, "full_output_path") || resultHas(r, "output_total_chars") {
		t.Fatalf("%v", r)
	}
}

func TestSpillIsRedacted(t *testing.T) {
	env, _ := spillEnv(t)
	env.Secrets.Set("ICEBATH_REDACT_SECRETS", "1")
	r := runTerminal(t, env, TerminalRequest{Command: "python3 -c \"print('sk-proj-' + 'a1B2c3D4e5F6g7H8i9J0' * 3); [print('pad', 'y'*90) for i in range(200)]\"", TaskID: "t-spill-3"})
	full, err := os.ReadFile(resultString(r, "full_output_path"))
	if err != nil {
		t.Fatalf("%v %v", err, r)
	}
	if strings.Contains(string(full), "a1B2c3D4e5F6g7H8i9J0a1B2c3D4e5F6g7H8i9J0") {
		t.Fatal("spill holds the secret")
	}
}

func TestFailedCommandStillGetsSpill(t *testing.T) {
	env, _ := spillEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "python3 -c \"[print('e'*90) for i in range(200)]; import sys; sys.exit(3)\"", TaskID: "t-spill-5"})
	if r["exit_code"] != float64(3) {
		t.Fatalf("%v", r)
	}
	if _, err := os.Stat(resultString(r, "full_output_path")); err != nil {
		t.Fatal(err)
	}
}

// test_terminal_cwd_echo.py
func cwdEchoEnv(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	env := newTerminalTestEnv(t, dir)
	local := NewLocalEnvironment(context.Background(), env, dir, 30, nil, true)
	t.Cleanup(func() { local.Cleanup(false) })
	cacheFakeEnv("default", local)
	return env
}

func TestCdReportsNewCwd(t *testing.T) {
	env := cwdEchoEnv(t)
	target := filepath.Join(t.TempDir(), "projdir")
	os.MkdirAll(target, 0o755)
	r := runTerminal(t, env, TerminalRequest{Command: "cd " + target, TaskID: "t-cwd-1"})
	if r["exit_code"] != float64(0) || realPathOf(resultString(r, "cwd")) != realPathOf(target) {
		t.Fatalf("%v", r)
	}
	r2 := runTerminal(t, env, TerminalRequest{Command: "pwd", TaskID: "t-cwd-1"})
	if resultHas(r2, "cwd") || realPathOf(strings.TrimSpace(resultString(r2, "output"))) != realPathOf(target) {
		t.Fatalf("stable cwd echoed: %v", r2)
	}
}

func TestNonCdCommandHasNoCwdField(t *testing.T) {
	env := cwdEchoEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "echo hello", TaskID: "t-cwd-2"})
	if r["exit_code"] != float64(0) || resultHas(r, "cwd") {
		t.Fatalf("%v", r)
	}
}

func TestCdWithinChainReportsFinalDir(t *testing.T) {
	env := cwdEchoEnv(t)
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(b, 0o755)
	r := runTerminal(t, env, TerminalRequest{Command: "cd " + a + " && cd " + b + " && echo done", TaskID: "t-cwd-4"})
	if r["exit_code"] != float64(0) || realPathOf(resultString(r, "cwd")) != realPathOf(b) {
		t.Fatalf("%v", r)
	}
}

func TestWorkdirOverrideDoesNotEcho(t *testing.T) {
	env := cwdEchoEnv(t)
	sub := filepath.Join(t.TempDir(), "transient")
	os.MkdirAll(sub, 0o755)
	r := runTerminal(t, env, TerminalRequest{Command: "echo hi", Workdir: sub, TaskID: "t-cwd-5"})
	if r["exit_code"] != float64(0) || resultHas(r, "cwd") {
		t.Fatalf("%v", r)
	}
	if _, ok := GetSessionCwd(env, "t-cwd-5"); ok {
		t.Fatal("transient workdir recorded as session cwd")
	}
}

func TestFailedCdNoEcho(t *testing.T) {
	env := cwdEchoEnv(t)
	r := runTerminal(t, env, TerminalRequest{Command: "cd /nonexistent_dir_zzz_42", TaskID: "t-cwd-6"})
	if r["exit_code"] == float64(0) || resultHas(r, "cwd") {
		t.Fatalf("%v", r)
	}
}
