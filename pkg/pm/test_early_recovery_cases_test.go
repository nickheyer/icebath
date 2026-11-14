package pm

import (
	"math/big"
	"os"
	"testing"
)

func TestPidLivenessRecognizesCurrentProcess(t *testing.T) {
	if live, err := pidRunning(big.NewInt(int64(os.Getpid()))); err != nil || !live {
		t.Fatalf("current process %v %v", live, err)
	}
	if live, err := pidRunning(big.NewInt(0)); err != nil || live {
		t.Fatalf("pid 0 %v %v", live, err)
	}
}

func TestProcessStateIsNoneForADeadPid(t *testing.T) {
	if state, known := processState(4294967294); known {
		t.Fatalf("an unprobeable pid must degrade to None (unknown), got %q", state)
	}
}
