package codeexec

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestWedgedLifecycleProbeReturnsBoundedErrorWithoutRunning(t *testing.T) {
	env := testEnv(t)
	writeKernelConfig(t, env, map[string]any{"timeout": 0.05})
	prevProbe, prevFloor, prevConfig := isSupervisedGatewayProcess, preExecGuardMinTimeoutS, getEnvConfig
	t.Cleanup(func() {
		isSupervisedGatewayProcess, preExecGuardMinTimeoutS, getEnvConfig = prevProbe, prevFloor, prevConfig
	})
	preExecGuardMinTimeoutS = 0
	isSupervisedGatewayProcess = func() bool { time.Sleep(time.Second); return false }
	ran := make(chan string, 1)
	getEnvConfig = func(*tools.Env) (map[string]any, error) {
		ran <- "env"
		return map[string]any{"env_type": "local"}, nil
	}
	start := time.Now()
	result := decodeResult(t, executeCode(t.Context(), env, "print('hi')", "", nil, false))
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("lifecycle probe wedged execute_code for %s", elapsed)
	}
	if !strings.Contains(errorOf(result), "did not finish") {
		t.Fatalf("result %v", result)
	}
	if len(ran) != 0 {
		t.Fatal("a probe with no verdict must not fail open into execution")
	}
}

func TestInterpreterKillInExecuteCodeNamesTheOwnedProcessRoute(t *testing.T) {
	env := testEnv(t)
	prev := isSupervisedGatewayProcess
	isSupervisedGatewayProcess = func() bool { return true }
	t.Cleanup(func() { isSupervisedGatewayProcess = prev })
	result := decodeResult(t, executeCode(t.Context(), env, `import subprocess; subprocess.run(["pkill", "-9", "python3"])`, "", nil, false))
	if !strings.Contains(errorOf(result), "proc_") || !strings.Contains(errorOf(result), "explicit PID") {
		t.Fatalf("kill result %v", result)
	}
	generic := decodeResult(t, executeCode(t.Context(), env, `import os; os.system("icebath gateway restart")`, "", nil, false))
	if strings.Contains(errorOf(generic), "proc_") || !strings.Contains(errorOf(generic), "cannot restart or stop the gateway") {
		t.Fatalf("generic result %v", generic)
	}
}

func TestTerminalEnvConfigUsesTerminalCwd(t *testing.T) {
	env := testEnv(t)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_ENV", "local")
	t.Setenv("TERMINAL_CWD", workspace)
	cfg, err := tools.GetEnvConfig(env)
	if err != nil || cfg["cwd"] != workspace {
		t.Fatalf("cwd %v err %v", cfg["cwd"], err)
	}
}

func TestExecuteCodeProjectModeFallsBackWhenTerminalCwdMissing(t *testing.T) {
	env := testEnv(t)
	base := t.TempDir()
	staging := filepath.Join(base, "staging")
	if err := os.Mkdir(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing")
	t.Setenv("TERMINAL_CWD", missing)
	resolved, err := resolveChildCwd(env, "project", staging, "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() || resolved == missing {
		t.Fatalf("resolved %q err %v", resolved, err)
	}
}

func TestBadImportProducesHintField(t *testing.T) {
	env := kernelEnv(t, nil)
	r := decodeResult(t, executeCode(t.Context(), env, "from icebath_tools import totally_fake_tool\nprint('unreachable')", "t-sbhint", nil, false))
	hint, ok := r["hint"].(string)
	if r["status"] != "error" || !ok || !strings.Contains(hint, "totally_fake_tool") {
		t.Fatalf("result %v", r)
	}
}

func TestSuccessfulScriptHasNoHint(t *testing.T) {
	env := kernelEnv(t, nil)
	r := decodeResult(t, executeCode(t.Context(), env, "print('fine')", "t-sbhint", nil, false))
	if _, ok := r["hint"]; r["status"] != "success" || ok {
		t.Fatalf("result %v", r)
	}
}

var truncationNotice = regexp.MustCompile(`\n\n\.\.\. \[(?P<label>[A-Z ]+) TRUNCATED - (?P<omitted>[\d,]+) (?P<unit>chars|bytes) ` +
	`omitted out of (?P<total>[\d,]+) total\] \.\.\.\n\n`)

func TestExecuteCodeStdoutTruncation(t *testing.T) {
	var b strings.Builder
	for i := range 2000 {
		b.WriteString(strconv.Itoa(10000 + i)[1:] + "|")
	}
	text := b.String()
	prevMax, prevSpill := maxStdoutBytes, spillFullStdout
	maxStdoutBytes = 1000
	spillFullStdout = func(*tools.Env, string) string { return "" }
	t.Cleanup(func() { maxStdoutBytes, spillFullStdout = prevMax, prevSpill })
	out, meta := truncateStdoutText(testEnv(t), text)
	notices := truncationNotice.FindAllStringSubmatchIndex(out, -1)
	if len(notices) != 1 {
		t.Fatalf("want one notice in %q", out[:200])
	}
	m := notices[0]
	group := func(name string) string {
		i := truncationNotice.SubexpIndex(name)
		return out[m[2*i]:m[2*i+1]]
	}
	if out[:m[0]] != text[:400] || out[m[1]:] != text[len(text)-600:] {
		t.Fatal("head or tail is not the 40/60 split of the budget")
	}
	if group("label") != "OUTPUT" || group("unit") != "bytes" || strings.ReplaceAll(group("omitted"), ",", "") != "9000" ||
		strings.ReplaceAll(group("total"), ",", "") != "10000" {
		t.Fatalf("notice %q", out[m[0]:m[1]])
	}
	if omitted, _ := meta.Get("stdout_bytes_omitted"); omitted != 9000 {
		t.Fatalf("meta %v", meta)
	}
}

func TestTZInjectedWhenConfigured(t *testing.T) {
	env := kernelEnv(t, nil)
	t.Setenv("ICEBATH_TIMEZONE", "Asia/Kolkata")
	probe := `import os; print("TZ=" + os.environ.get("TZ", "NOT_SET")); ` +
		`print("ICEBATH_TIMEZONE=" + os.environ.get("ICEBATH_TIMEZONE", "NOT_SET"))`
	result := decodeResult(t, executeCode(t.Context(), env, probe, "tz-combined-test", []string{}, false))
	wantStatus(t, result, "success")
	if !strings.Contains(outputOf(result), "TZ=Asia/Kolkata") || !strings.Contains(outputOf(result), "ICEBATH_TIMEZONE=NOT_SET") {
		t.Fatalf("result %v", result)
	}
}

// Tree kill that raises the way a PermissionError escapes kill_process_tree
func withRaisingTreeKill(t *testing.T) {
	prev := killProcessTree
	killProcessTree = func(int, syscall.Signal) bool { panic(&os.PathError{Op: "kill", Err: os.ErrPermission}) }
	t.Cleanup(func() { killProcessTree = prev })
}

func TestSwallowsDelegationRaiseFallsBackToPlainKill(t *testing.T) {
	withRaisingTreeKill(t)
	kills := 0
	killProcessGroup(7777, func() error { kills++; return nil }, nil, false)
	if kills != 1 {
		t.Fatalf("plain kill ran %d times", kills)
	}
}

func TestEvenProcKillRaiseIsSwallowed(t *testing.T) {
	withRaisingTreeKill(t)
	killProcessGroup(8888, func() error { return os.ErrProcessDone }, nil, false)
}
