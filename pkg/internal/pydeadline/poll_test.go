package pydeadline

import (
	"testing"
	"time"
)

func TestPollUntilReturnsTheFirstHoldingResultOrTheLastAtTimeout(t *testing.T) {
	n := 0
	if got := PollUntil(func() (int, bool) { n++; return n, n == 3 }, time.Second, time.Millisecond); got != 3 {
		t.Fatalf("held %d", got)
	}
	start := time.Now()
	if got := PollUntil(func() (string, bool) { return "last", false }, 30*time.Millisecond, 10*time.Millisecond); got != "last" {
		t.Fatalf("timeout %q", got)
	}
	if time.Since(start) < 30*time.Millisecond {
		t.Fatal("returned before the timeout")
	}
}
