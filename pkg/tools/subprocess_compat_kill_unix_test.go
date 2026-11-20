//go:build !windows

package tools

import (
	"math"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestPidExistsStdlibPosixVerdicts(t *testing.T) {
	for _, pid := range []int{os.Getpid(), 0} {
		if alive, err := PidExistsStdlib(pid); err != nil || !alive {
			t.Fatalf("pid %d reported %v %v, want alive", pid, alive, err)
		}
	}
	if _, err := PidExistsStdlib(math.MaxInt32 + 1); err == nil {
		t.Fatal("a pid past the C int range must raise as os.kill does")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if zombie, err := posixIsZombie(pid); err != nil {
			t.Fatal(err)
		} else if zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unreaped child never became a zombie")
		}
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("zombie must still answer a null signal, got %v", err)
	}
	if alive, err := PidExistsStdlib(pid); err != nil || alive {
		t.Fatalf("zombie reported %v %v, want dead", alive, err)
	}
	_ = cmd.Wait()
	if alive, err := PidExistsStdlib(pid); err != nil || alive {
		t.Fatalf("reaped pid reported %v %v, want dead", alive, err)
	}
}

func TestKillAndDrainReturnsWhatTheKilledGroupLeftInItsPipes(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo out; echo err >&2; sleep 300 & sleep 300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child, err := StartPiped(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := child.Communicate(300 * time.Millisecond); ok {
		t.Fatal("a stalled child must time out its bounded communicate")
	}
	stdout, stderr, ok := KillAndDrain(child, 5*time.Second)
	if !ok || string(stdout) != "out\n" || string(stderr) != "err\n" {
		t.Fatalf("drain %q %q %v", stdout, stderr, ok)
	}
}
