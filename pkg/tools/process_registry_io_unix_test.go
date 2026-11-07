//go:build unix

package tools

import (
	"os/exec"
	"testing"
	"time"
)

func TestTerminateHostPidWithNilEnvEndsTheProcess(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	TerminateHostPid(nil, cmd.Process.Pid, nil)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process survived TerminateHostPid")
	}
}

func TestTerminateHostPidRefusesARecycledPid(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	wrong := int64(1)
	TerminateHostPid(&Env{Home: t.TempDir()}, cmd.Process.Pid, &wrong)
	if !procAlive(cmd.Process.Pid) {
		t.Fatal("start time mismatch still terminated the process")
	}
}
