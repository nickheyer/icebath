package commands

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestBangDetectionLeadingBangIsBang(t *testing.T) {
	for _, text := range []string{"!ls", "!git status", "  !ls -la", "!  spaced out", "!!", "!"} {
		if !IsBangCommand(text) {
			t.Errorf("IsBangCommand(%q) = false", text)
		}
	}
}

func TestBangDetectionNonLeadingBangIsNotBang(t *testing.T) {
	for _, text := range []any{"fix the bug!", "echo hi! please", "run this: !ls", "/help", "hello world", "", "   ", nil, 123} {
		if IsBangCommand(text) {
			t.Errorf("IsBangCommand(%#v) = true", text)
		}
	}
}

func TestBangDetectionParseStripsExactlyOneBang(t *testing.T) {
	cases := [][2]string{{"!ls", "ls"}, {"!git status", "git status"}, {"  !ls -la  ", "ls -la"}, {"!   echo hi", "echo hi"},
		{"!!", "!"}, {"!!ls", "!ls"}, {"!", ""}, {"!   ", ""}, {"not a bang", ""}}
	for _, c := range cases {
		if got := ParseBangCommand(c[0]); got != c[1] {
			t.Errorf("ParseBangCommand(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func clearBangContextEnv(t *testing.T) {
	for _, v := range []string{"ICEBATH_GATEWAY_SESSION", "ICEBATH_CRON_SESSION", "ICEBATH_SESSION_PLATFORM"} {
		t.Setenv(v, "")
	}
}

func TestBangContextGatingEnabledInPlainCli(t *testing.T) {
	clearBangContextEnv(t)
	if !BangShellEnabled() {
		t.Fatal("bang shell disabled in a plain CLI")
	}
}

func TestBangContextGatingDisabledInNonCliContexts(t *testing.T) {
	for _, c := range [][2]string{{"ICEBATH_GATEWAY_SESSION", "1"}, {"ICEBATH_CRON_SESSION", "true"}, {"ICEBATH_SESSION_PLATFORM", "discord"}} {
		t.Run(c[0], func(t *testing.T) {
			clearBangContextEnv(t)
			t.Setenv(c[0], c[1])
			if BangShellEnabled() {
				t.Fatalf("bang shell enabled with %s=%s", c[0], c[1])
			}
		})
	}
}

func runBang(t *testing.T, command, cwd string) ([]string, int) {
	t.Helper()
	var lines []string
	code := RunBangCommand(context.Background(), nil, command, cwd, BangDefaultTimeout, func(l string) { lines = append(lines, l) })
	return lines, code
}

func TestBangExecutionOutputIsStreamedToWriter(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	lines, code := runBang(t, "echo bang-one; echo bang-two", "")
	if code != 0 || !slices.Contains(lines, "bang-one") || !slices.Contains(lines, "bang-two") {
		t.Fatalf("code %d lines %q", code, lines)
	}
}

func TestBangExecutionStderrIsMergedIntoOutput(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	lines, _ := runBang(t, "echo to-stderr >&2", "")
	if !slices.Contains(lines, "to-stderr") {
		t.Fatalf("lines %q", lines)
	}
}

func TestBangExecutionNonzeroExitCodeIsReturned(t *testing.T) {
	if _, code := runBang(t, "exit 42", ""); code != 42 {
		t.Fatalf("code %d", code)
	}
}

func TestBangExecutionRunsInRequestedCwd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	dir := t.TempDir()
	lines, code := runBang(t, "pwd", dir)
	want, _ := filepath.EvalSymlinks(dir)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(lines[len(lines)-1]))
	if code != 0 || got != want {
		t.Fatalf("code %d pwd %q want %q", code, got, want)
	}
}

func TestBangExecutionMissingCwdFallsBackWithoutCrashing(t *testing.T) {
	lines, code := runBang(t, "echo ok", filepath.Join(t.TempDir(), "does-not-exist"))
	if code != 0 || !slices.Contains(lines, "ok") {
		t.Fatalf("code %d lines %q", code, lines)
	}
}

func TestBangExecutionUnavailableEnvironmentSanitizerPreventsLaunch(t *testing.T) {
	prevEnv, prevShell := bangEnv, bangShellCommand
	t.Cleanup(func() { bangEnv, bangShellCommand = prevEnv, prevShell })
	bangEnv = func(context.Context, *tools.Env) (map[string]string, error) {
		return nil, errors.New("environment sanitizer unavailable")
	}
	popen := 0
	bangShellCommand = func(command string) (*exec.Cmd, error) {
		popen++
		return prevShell(command)
	}
	lines, code := runBang(t, "echo must-not-run", "")
	if code != 127 || popen != 0 {
		t.Fatalf("code %d popen calls %d", code, popen)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "failed to run command") }) {
		t.Fatalf("lines %q", lines)
	}
}

func TestRunBangCommandSplitsUniversalNewlinesAndReplacesInvalidUTF8(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("printf is a posix shell builtin")
	}
	lines, code := runBang(t, `printf 'a\r\nb\rc\n\377d\ne'`, "")
	if code != 0 || !slices.Equal(lines, []string{"a", "b", "c", "�d", "e"}) {
		t.Fatalf("code %d lines %q", code, lines)
	}
}

func TestRunBangCommandReportsInterruptAndSignalReturncode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix signals")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var lines []string
	done := make(chan int, 1)
	go func() {
		done <- RunBangCommand(ctx, nil, "echo started; sleep 30", "", BangDefaultTimeout, func(l string) {
			lines = append(lines, l)
			if l == "started" {
				cancel()
			}
		})
	}()
	if code := <-done; code != 130 || !slices.Equal(lines, []string{"started", "!: interrupted"}) {
		t.Fatalf("code %d lines %q", code, lines)
	}
	if _, code := runBang(t, "kill -9 $$", ""); code != -9 {
		t.Fatalf("signal returncode %d", code)
	}
}

func TestRunBangCommandTimesOutAfterOutputCloses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell redirection")
	}
	var lines []string
	code := RunBangCommand(context.Background(), nil, "exec >/dev/null 2>&1; sleep 30", "", 1, func(l string) { lines = append(lines, l) })
	if code != 124 || !slices.Equal(lines, []string{"!: command timed out after 1s"}) {
		t.Fatalf("code %d lines %q", code, lines)
	}
}
