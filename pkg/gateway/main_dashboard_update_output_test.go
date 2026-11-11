//go:build !windows

package gateway

import (
	"bytes"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Stream whose terminal is gone, every write and flush failing with a broken pipe
type brokenStream struct{}

func (brokenStream) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "terminal", Err: syscall.EPIPE}
}
func (brokenStream) Flush() error { return errors.New("terminal gone") }

func TestUpdateOutputStreamWriteContinuesAfterBrokenOriginal(t *testing.T) {
	var log bytes.Buffer
	stream := NewUpdateOutputStream(brokenStream{}, &log)
	for _, line := range []string{"first line\n", "second line\n"} {
		if n, err := stream.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	if log.String() != "first line\nsecond line\n" {
		t.Fatalf("log = %q", log.String())
	}
	if !stream.OriginalBroken() {
		t.Fatal("the broken flag was not set")
	}
}

func TestInstallHangupProtectionWrapsStdoutAndStderrWithMirror(t *testing.T) {
	tmp := t.TempDir()
	swapSeam(t, &updateOutputRootHome, func() string { return tmp })
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	var prevOut, prevErr bytes.Buffer
	state := InstallHangupProtection(&prevOut, &prevErr, false)
	if !state.Installed || state.LogFile == nil {
		t.Fatalf("state = %+v", state)
	}
	out, isWrapped := state.Stdout.(*UpdateOutputStream)
	if _, errWrapped := state.Stderr.(*UpdateOutputStream); !isWrapped || !errWrapped {
		t.Fatalf("streams = %T, %T", state.Stdout, state.Stderr)
	}
	if _, err := out.Write([]byte("checking mirror\n")); err != nil {
		t.Fatal(err)
	}
	out.Flush()
	contents, err := os.ReadFile(filepath.Join(tmp, "logs", "update.log"))
	if err != nil || !strings.Contains(string(contents), "checking mirror") {
		t.Fatalf("log = %q err = %v", contents, err)
	}
	FinalizeUpdateOutput(state)
	if state.Stdout != &prevOut || state.Stderr != &prevErr {
		t.Fatal("the streams were not restored")
	}
}

func TestInstallHangupProtectionNonFatalIfLogSetupFails(t *testing.T) {
	swapSeam(t, &updateOutputRootHome, func() string { panic("no home for you") })
	t.Cleanup(func() { signal.Reset(syscall.SIGHUP) })
	var prevOut, prevErr bytes.Buffer
	state := InstallHangupProtection(&prevOut, &prevErr, false)
	if state.Stdout != &prevOut || state.Stderr != &prevErr || state.Installed {
		t.Fatalf("state = %+v", state)
	}
	if !signal.Ignored(syscall.SIGHUP) {
		t.Fatal("SIGHUP is not ignored")
	}
	FinalizeUpdateOutput(state)
}

func TestFinalizeUpdateOutputSkippedInstallLeavesStdioAlone(t *testing.T) {
	var sentinel, current bytes.Buffer
	state := &UpdateOutputState{PrevStdout: &sentinel, PrevStderr: &sentinel, Stdout: &current, Stderr: &current}
	FinalizeUpdateOutput(state)
	if state.Stdout != &current || state.Stderr != &current {
		t.Fatal("an uninstalled state had its streams replaced")
	}
}
