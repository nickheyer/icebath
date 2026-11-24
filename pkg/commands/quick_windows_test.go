//go:build windows

package commands

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	executil "github.com/nickheyer/icebath/pkg/utils/exec"
)

func TestQuickExecRepliesOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	if got, err := RunGatewayExecQuickCommand(ctx, env, "echo a& echo b", ""); err != nil || got != "a \r\nb" {
		t.Fatalf("gateway %q %v", got, err)
	}
	if got, err := RunGatewayExecQuickCommand(ctx, env, "rem", ""); err != nil || got != "Command returned no output." {
		t.Fatalf("gateway no output %q %v", got, err)
	}
}

func TestQuickExecFailuresOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	missing := filepath.Join(t.TempDir(), "missing")
	shell, err := executil.Shell()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := RunGatewayExecQuickCommand(ctx, env, "echo hi", missing); got != "Quick command error: "+(&fs.PathError{Op: "fork/exec", Path: shell, Err: windows.ERROR_DIRECTORY}).Error() {
		t.Fatalf("missing cwd %q", got)
	}
	if got, _ := RunGatewayExecQuickCommand(ctx, env, []byte("echo hi"), ""); got != "Quick command error: bytes args is not allowed on Windows" {
		t.Fatalf("gateway bytes %q", got)
	}
	t.Setenv("ComSpec", "")
	t.Setenv("SystemRoot", "")
	if got, _ := RunGatewayExecQuickCommand(ctx, env, "echo hi", ""); got != "Quick command error: no command shell found: ComSpec is unset and SystemRoot is not an absolute directory" {
		t.Fatalf("no shell %q", got)
	}
}

func TestQuickExecTimeoutAndCancellationOnWindows(t *testing.T) {
	ctx := quickTestContext(t)
	env := quickToolEnv()
	setQuickTimeout(t, &execQuickCommandTimeout, time.Second)
	start := time.Now()
	if got, err := RunGatewayExecQuickCommand(ctx, env, "ping -n 30 127.0.0.1 >nul", ""); err != nil || got != "Quick command timed out (30s)." || time.Since(start) > 20*time.Second {
		t.Fatalf("gateway timeout %q %v after %s", got, err, time.Since(start))
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
