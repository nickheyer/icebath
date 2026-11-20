//go:build linux || darwin

package kanban

import (
	"os/exec"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

func TestAnExitedUnreapedOwnerNoLongerHoldsTheSlot(t *testing.T) {
	b := newOwnerBoard(t)
	child := exec.Command("cat")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	birth, err := proc.WorkerBirth(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st, err := proc.State(child.Process.Pid); err == nil && st == 'Z' {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.deadAttempt("t_ghost", deadAttempt{ownerPid: intp(child.Process.Pid), ownerBirth: birth})
	if n := mustRelease(t, b.conn, nil); n != 1 || b.state("t_ghost").Get("status") != "ready" {
		t.Fatal("zombie owner still holds the slot", n)
	}
}
