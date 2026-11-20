//go:build unix

package proc

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestStateTracksStopAndExit(t *testing.T) {
	if s, err := State(os.Getpid()); err != nil || (s != 'R' && s != 'S') {
		t.Fatalf("self state %q %v", s, err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(pid, syscall.SIGSTOP)
	want := func(code byte) {
		end := time.Now().Add(5 * time.Second)
		for {
			s, err := State(pid)
			if err == nil && s == code {
				return
			}
			if time.Now().After(end) {
				t.Fatalf("want %q got %q %v", code, s, err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	want('T')
	_ = syscall.Kill(pid, syscall.SIGKILL)
	want('Z')
	_ = cmd.Wait()
	if _, err := State(pid); err == nil {
		t.Fatal("reaped pid still has a state")
	}
}
