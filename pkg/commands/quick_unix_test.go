//go:build !windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
)

const quickTree = `echo $$ > shell.pid; sleep 120 & echo $! > child.pid; wait`

// Whether pid names a process that is not a zombie
func quickAlive(pid int) bool {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(data)
		i := strings.LastIndexByte(s, ')')
		return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
	}
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// Pid written to path, zero while the file is missing or empty
func quickPid(path string) int {
	data, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return n
}

// First nonzero value of pred within timeout, failing the test with what otherwise
func quickWaitUntil(t *testing.T, pred func() int, timeout time.Duration, what string) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if v := pred(); v != 0 {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func killLeftover(t *testing.T, pid int) {
	t.Cleanup(func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestTimedOutExecCommandKillsItsBackgroundChild(t *testing.T) {
	ctx := quickTestContext(t)
	setQuickTimeout(t, &execQuickCommandTimeout, 2*time.Second)
	dir := t.TempDir()
	reply, err := RunGatewayExecQuickCommand(ctx, quickToolEnv(), quickTree, dir)
	if err != nil || reply != i18n.T(ctx, "gateway.quick_command.timed_out", "", nil) || reply != "Quick command timed out (30s)." {
		t.Fatalf("reply %q err %v", reply, err)
	}
	child := quickPid(filepath.Join(dir, "child.pid"))
	if child == 0 {
		t.Fatal("child pid missing")
	}
	killLeftover(t, child)
	quickWaitUntil(t, func() int {
		if quickAlive(child) {
			return 0
		}
		return 1
	}, 10*time.Second, "the backgrounded sleep to stop")
}

func TestCancelledExecCommandKillsShellAndChild(t *testing.T) {
	base := quickTestContext(t)
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := RunGatewayExecQuickCommand(ctx, quickToolEnv(), quickTree, dir)
		done <- err
	}()
	child := quickWaitUntil(t, func() int { return quickPid(filepath.Join(dir, "child.pid")) }, 10*time.Second, "the child pid")
	shell := quickPid(filepath.Join(dir, "shell.pid"))
	killLeftover(t, child)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run returned %v", err)
	}
	quickWaitUntil(t, func() int {
		if quickAlive(shell) || quickAlive(child) {
			return 0
		}
		return 1
	}, 10*time.Second, "the shell and its child to stop")
}

func TestGatewayExecQuickCommandReplies(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	cases := []struct {
		cmd  any
		want string
	}{
		{"echo ok", "ok"},
		{"printf '  padded \\n\\n'", "padded"},
		{"echo out; echo err >&2; exit 3", "out"},
		{"echo only-err >&2", "only-err"},
		{"true", "Command returned no output."},
		{[]byte("echo bytes"), "bytes"},
		{"printf 'a\\r\\nb\\rc\\r\\n'", "a\r\nb\rc"},
		{"printf 'ok\\377\\342\\202x'", "Quick command error: 'utf-8' codec can't decode byte 0xff in position 2: invalid start byte"},
		{"printf '\\342\\202'", "Quick command error: 'utf-8' codec can't decode bytes in position 0-1: unexpected end of data"},
	}
	for _, c := range cases {
		if got, err := RunGatewayExecQuickCommand(ctx, env, c.cmd, ""); err != nil || got != c.want {
			t.Fatalf("%v replied %q err %v want %q", c.cmd, got, err, c.want)
		}
	}
}

func TestQuickWhitespaceStdoutIsNoOutputOnTheGateway(t *testing.T) {
	ctx := quickTestContext(t)
	cmd := "printf ' \\n\\t'; echo from-stderr >&2"
	if got, err := RunGatewayExecQuickCommand(ctx, quickToolEnv(), cmd, ""); err != nil || got != "Command returned no output." {
		t.Fatalf("gateway %q %v", got, err)
	}
}

func TestQuickExecScrubsCredentialsAndRedactsOutput(t *testing.T) {
	ctx := quickTestContext(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-secret-12345")
	got, err := RunGatewayExecQuickCommand(ctx, quickToolEnv(), "env", "")
	if err != nil || strings.Contains(got, "sk-or-secret-12345") || !strings.Contains(got, "PATH=") {
		t.Fatalf("gateway env leaked or did not run %v %q", err, got)
	}
	secret := "echo sk-ant-api03-supersecretkey1234567890"
	if got, _ = RunGatewayExecQuickCommand(ctx, quickToolEnv(), secret, ""); strings.Contains(got, "supersecretkey1234567890") || got == "" {
		t.Fatalf("gateway output not redacted %q", got)
	}
}

func TestGatewayExecQuickCommandCancelledBeforeStartNeverSpawns(t *testing.T) {
	base := quickTestContext(t)
	ctx, cancel := context.WithCancel(base)
	cancel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	if got, err := RunGatewayExecQuickCommand(ctx, quickToolEnv(), "touch "+marker, dir); got != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre cancelled run %q %v", got, err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a cancelled task spawned its shell")
	}
}

func TestQuickSpawnFailures(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gateway := []struct {
		cmd  any
		cwd  string
		want string
	}{
		{"echo hi", missing, "Quick command error: [Errno 2] No such file or directory: '" + missing + "'"},
		{"echo hi", file, "Quick command error: [Errno 20] Not a directory: '" + file + "'"},
		{"echo\x00hi", "", "Quick command error: embedded null byte"},
		{"echo hi", dir + "\x00x", "Quick command error: embedded null byte"},
		{5, "", "Quick command error: cmd must be a string"},
		{[]any{"echo", "hi"}, "", "Quick command error: cmd must be a string"},
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o600); err != nil {
			t.Fatal(err)
		}
		gateway = append(gateway, struct {
			cmd  any
			cwd  string
			want string
		}{"echo hi", locked, "Quick command error: [Errno 13] Permission denied: '" + locked + "'"})
	}
	for _, c := range gateway {
		if got, err := RunGatewayExecQuickCommand(ctx, env, c.cmd, c.cwd); err != nil || got != c.want {
			t.Fatalf("gateway %q in %q replied %q err %v want %q", c.cmd, c.cwd, got, err, c.want)
		}
	}
}

func TestQuickReplyKeysPerLocale(t *testing.T) {
	ctx := quickTestContext(t)
	setQuickTimeout(t, &execQuickCommandTimeout, 50*time.Millisecond)
	env := quickToolEnv()
	english := map[string]string{
		"gateway.quick_command.no_output": "Command returned no output.",
		"gateway.quick_command.timed_out": "Quick command timed out (30s).",
		"gateway.quick_command.error":     "Quick command error: {error}",
	}
	for lang := range i18n.BundledLanguageInfo {
		t.Setenv("ICEBATH_LANGUAGE", lang)
		catalog := i18n.LoadBundled(lang)
		want := func(key string, kwargs map[string]any) string {
			t.Helper()
			if catalog[key] == "" {
				t.Fatalf("%s lacks %s", lang, key)
			}
			if lang == "en" && catalog[key] != english[key] {
				t.Fatalf("en %s is %q", key, catalog[key])
			}
			out, err := i18n.PyFormat(catalog[key], kwargs)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
		gw := func(cmd any) string {
			got, err := RunGatewayExecQuickCommand(ctx, env, cmd, "")
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		checks := []struct{ got, want string }{
			{gw("true"), want("gateway.quick_command.no_output", nil)},
			{gw("sleep 5"), want("gateway.quick_command.timed_out", nil)},
			{gw(5), want("gateway.quick_command.error", map[string]any{"error": "cmd must be a string"})},
		}
		for i, c := range checks {
			if c.got != c.want {
				t.Fatalf("%s check %d got %q want %q", lang, i, c.got, c.want)
			}
		}
	}
}

func TestQuickChildEnvKeepsTheInsertionOrderOfItsNames(t *testing.T) {
	ctx := quickTestContext(t)
	t.Setenv("ZED_ORDER_PROBE", "1")
	t.Setenv("ALPHA_ORDER_PROBE", "2")
	t.Setenv("MID_ORDER_PROBE", "3")
	child, err := quickChildEnv(ctx, quickToolEnv())
	if err != nil {
		t.Fatal(err)
	}
	position := map[string]int{}
	for i, k := range child.Keys {
		position[k] = i
	}
	if len(position) != len(child.Keys) || len(child.Keys) != len(child.Vars) {
		t.Fatalf("keys %d positions %d vars %d", len(child.Keys), len(position), len(child.Vars))
	}
	zed, alpha, mid := position["ZED_ORDER_PROBE"], position["ALPHA_ORDER_PROBE"], position["MID_ORDER_PROBE"]
	if child.Vars["ZED_ORDER_PROBE"] != "1" || !(zed < alpha && alpha < mid) {
		t.Fatalf("probe positions %d %d %d", zed, alpha, mid)
	}
	inherited := []string{}
	seen := map[string]bool{}
	for _, kv := range os.Environ() {
		name := kv[:strings.IndexByte(kv, '=')]
		if _, kept := child.Vars[name]; kept && !seen[name] {
			seen[name] = true
			inherited = append(inherited, name)
		}
	}
	if !reflect.DeepEqual(child.Keys[:len(inherited)], inherited) {
		t.Fatalf("inherited names %v, want the process order %v", child.Keys[:len(inherited)], inherited)
	}
	for _, name := range child.Keys[len(inherited):] {
		if seen[name] {
			t.Fatalf("%s listed twice", name)
		}
	}
	list := quickEnvList(child)
	for i, k := range child.Keys {
		if list[i] != k+"="+child.Vars[k] {
			t.Fatalf("entry %d is %q", i, list[i])
		}
	}
}
