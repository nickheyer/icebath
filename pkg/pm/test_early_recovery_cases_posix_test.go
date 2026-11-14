//go:build linux || darwin

package pm

import (
	"math/big"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPidLivenessCountsAZombieAsDead(t *testing.T) {
	child := exec.Command("true")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	becameZombie := false
	for range 40 {
		if state, known := processState(pid); known && strings.HasPrefix(strings.ToUpper(state), "Z") {
			becameZombie = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !becameZombie {
		_ = child.Wait()
		t.Fatal("child never reached zombie state on this platform")
	}
	live, err := pidRunning(big.NewInt(int64(pid)))
	_ = child.Wait()
	if err != nil || live {
		t.Fatalf("a zombie is not a live owner %v %v", live, err)
	}
}

func TestProcessStateReportsALetterForALivePid(t *testing.T) {
	state, known := processState(os.Getpid())
	if !known || len([]rune(state)) != 1 {
		t.Fatalf("a live pid must expose a state, got %q %v", state, known)
	}
	if strings.HasPrefix(strings.ToUpper(state), "Z") {
		t.Fatal("this process is not a zombie")
	}
}
