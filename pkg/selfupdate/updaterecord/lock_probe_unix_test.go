//go:build !windows

package updaterecord

import (
	"math"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

func TestStdlibCreateTimeReadsThePsutilClock(t *testing.T) {
	birth, err := proc.Birth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	stdlib := stdlibCreateTime(os.Getpid())
	if stdlib == nil || math.Abs(*stdlib-birth) > CreateTimeToleranceSeconds {
		t.Fatalf("stdlib %v psutil %v", stdlib, birth)
	}
	if stdlibCreateTime(math.MaxInt32) != nil {
		t.Fatal("a pid that names no process has a creation time")
	}
}

func TestPidAliveRefusesAPidBeyondPidT(t *testing.T) {
	if pidAlive(1<<32 + os.Getpid()) {
		t.Fatal("an overflowing pid probed a truncated one")
	}
	if !pidAlive(os.Getpid()) {
		t.Fatal("this process reads dead")
	}
}
