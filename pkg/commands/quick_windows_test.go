//go:build windows

package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQuickExecRepliesOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	if got, err := RunGatewayExecQuickCommand(ctx, env, "echo a& echo b", ""); err != nil || got != "a \r\nb" {
		t.Fatalf("gateway %q %v", got, err)
	}
	if got, err := RunCLIExecQuickCommand(ctx, env, "echo a& echo b"); err != nil || got != (CLIQuickReply{"a \nb", CLIQuickOutput}) {
		t.Fatalf("cli %+v %v", got, err)
	}
	if got, err := RunCLIExecQuickCommand(ctx, env, []any{"echo", "hi there"}); err != nil || got != (CLIQuickReply{`"hi there"`, CLIQuickOutput}) {
		t.Fatalf("cli list2cmdline %+v %v", got, err)
	}
	if got, err := RunGatewayExecQuickCommand(ctx, env, "rem", ""); err != nil || got != "Command returned no output." {
		t.Fatalf("gateway no output %q %v", got, err)
	}
}

func TestQuickExecFailuresOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	missing := filepath.Join(t.TempDir(), "missing")
	if got, _ := RunGatewayExecQuickCommand(ctx, env, "echo hi", missing); got != "Quick command error: [WinError 267] The directory name is invalid" {
		t.Fatalf("missing cwd %q", got)
	}
	if got, _ := RunGatewayExecQuickCommand(ctx, env, []byte("echo hi"), ""); got != "Quick command error: bytes args is not allowed on Windows" {
		t.Fatalf("gateway bytes %q", got)
	}
	if got, _ := RunCLIExecQuickCommand(ctx, env, []byte("echo hi")); got != (CLIQuickReply{"Quick command error: bytes args is not allowed on Windows", CLIQuickFailure}) {
		t.Fatalf("cli bytes %+v", got)
	}
	if got, _ := RunCLIExecQuickCommand(ctx, env, "echo\x00hi"); got != (CLIQuickReply{"Quick command error: embedded null character", CLIQuickFailure}) {
		t.Fatalf("cli nul %+v", got)
	}
	if got, _ := RunCLIExecQuickCommand(ctx, env, []any{"echo", 5}); got != (CLIQuickReply{"Quick command error: expected str, bytes or os.PathLike object, not int", CLIQuickFailure}) {
		t.Fatalf("cli list item %+v", got)
	}
	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", "")
	if got, _ := RunGatewayExecQuickCommand(ctx, env, "echo hi", ""); got != "Quick command error: shell not found: neither %ComSpec% nor %SystemRoot% is set" {
		t.Fatalf("no shell %q", got)
	}
}

func TestQuickExecTimeoutAndCancellationOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	setQuickTimeout(t, &execQuickCommandTimeout, time.Second)
	setQuickTimeout(t, &cliQuickCommandTimeout, time.Second)
	start := time.Now()
	if got, err := RunGatewayExecQuickCommand(ctx, env, "ping -n 30 127.0.0.1 >nul", ""); err != nil || got != "Quick command timed out (30s)." || time.Since(start) > 20*time.Second {
		t.Fatalf("gateway timeout %q %v after %s", got, err, time.Since(start))
	}
	start = time.Now()
	if got, err := RunCLIExecQuickCommand(ctx, env, "ping -n 30 127.0.0.1 >nul"); err != nil || got != (CLIQuickReply{"Quick command timed out (30s)", CLIQuickFailure}) {
		t.Fatalf("cli timeout %+v %v after %s", got, err, time.Since(start))
	}
	cctx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()
	if _, err := RunGatewayExecQuickCommand(cctx, env, "ping -n 30 127.0.0.1 >nul", os.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled %v", err)
	}
}
