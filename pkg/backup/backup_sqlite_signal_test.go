//go:build !windows

package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysqlite"
	"github.com/nickheyer/icebath/pkg/state"
)

// Names of the staging copies under a home
func stagingNames(t *testing.T, h string) []string {
	return globNames(t, h, "state.db.pre-update-emergency-*.partial*")
}

// Polls a condition until it holds or the timeout passes
func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSIGTERMMidCopyRemovesTheStagingCopy(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	db := filepath.Join(h, "state.db")
	messagesDB(t, db, "DELETE", 10)
	holder := testConn(t, db, "")
	mustExec(t, holder, "BEGIN EXCLUSIVE")
	defer mustExec(t, holder, "ROLLBACK")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), preflightChildEnv+"="+h)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill()
	waitUntil(t, 30*time.Second, "the staging copy", func() bool { return len(stagingNames(t, h)) > 0 })
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-exited:
	case <-time.After(60 * time.Second):
		t.Fatal("child never stopped")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Fatalf("exit %v: %s", err, stderr.String())
	}
	if stdout.Len() != 0 || len(stagingNames(t, h)) != 0 {
		t.Fatalf("stdout %q staging %v", stdout.String(), stagingNames(t, h))
	}
}

func TestSIGTERMInterruptsAOneStepWALCopy(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.MkdirAll(h, 0o755)
	messagesDB(t, filepath.Join(h, "state.db"), "WAL", 200)
	started, release := make(chan struct{}), make(chan struct{})
	real := connBackup
	connBackup = func(src, dst pysqlite.RawConn, pages int, progress func(int, int, int) error, sleep time.Duration) error {
		close(started)
		<-release
		return real(src, dst, pages, progress, sleep)
	}
	t.Cleanup(func() { connBackup = real })
	defer close(release)
	type outcome struct {
		code   int
		stdout string
	}
	done := make(chan outcome, 1)
	go func() {
		code, stdout, _ := runPreflight(context.Background(), h)
		done <- outcome{code, stdout}
	}()
	<-started
	if len(stagingNames(t, h)) == 0 {
		t.Fatal("no staging copy while the copy runs")
	}
	signalled := time.Now()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the stop never landed while the one step copy ran")
	}
	if latency := time.Since(signalled); latency > 5*time.Second {
		t.Fatalf("stop latency %v", latency)
	}
	if got.code != 128+int(syscall.SIGTERM) || got.stdout != "" || len(stagingNames(t, h)) != 0 {
		t.Fatalf("exit %d stdout %q staging %v", got.code, got.stdout, stagingNames(t, h))
	}
}

func TestIsZeroedNeverProbesSpecialFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "state.db")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if state.IsZeroedStateDB(fifo, 100, false) {
		t.Fatal("fifo classified as zeroed")
	}
}
