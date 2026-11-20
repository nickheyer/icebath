package pythread

import (
	"runtime"
	"testing"
	"time"
)

func TestMonotonicCountsFromBootAndAdvancesWithTime(t *testing.T) {
	first := Monotonic()
	if first < time.Since(processStart).Seconds() {
		t.Fatalf("reading %v is behind the process clock", first)
	}
	if (runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows") && systemMonotonic() <= 0 {
		t.Fatal("the system monotonic clock read zero")
	}
	time.Sleep(20 * time.Millisecond)
	second := Monotonic()
	if delta := second - first; delta < 0.02 || delta > 5 {
		t.Fatalf("20ms of sleep moved the clock by %v", delta)
	}
	if runtime.GOOS == "linux" && systemMonotonic()-Monotonic() > 1 {
		t.Fatalf("clock drifted from CLOCK_MONOTONIC: %v against %v", Monotonic(), systemMonotonic())
	}
}
