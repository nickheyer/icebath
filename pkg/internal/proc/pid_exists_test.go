package proc

import (
	"os"
	"runtime"
	"testing"
)

func TestPidExistsSeesSelfAndRejectsNegative(t *testing.T) {
	if live, err := PidExists(os.Getpid()); err != nil || !live {
		t.Fatalf("own pid = %v, %v", live, err)
	}
	if live, err := PidExists(-1); err != nil || live {
		t.Fatalf("negative pid = %v, %v", live, err)
	}
	if !SignalProbeLive(os.Getpid()) && runtime.GOOS != "windows" {
		t.Fatal("signal probe misses this process")
	}
}
