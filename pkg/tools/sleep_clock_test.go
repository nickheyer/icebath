package tools

import (
	"math"
	"testing"
	"time"
)

func TestSleepWatchCountsOnlyStopsOfFiveSecondsOrMore(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	w := NewSleepWatch(0.3)
	if got := w.Slept(); got != 0 {
		t.Fatalf("an immediate check reported %v seconds stopped", got)
	}
	w.wall -= 4.0
	if got := w.Slept(); got != 0 {
		t.Fatalf("a stop under five seconds counted as %v", got)
	}
	w.wall -= 12.3
	if got := w.Slept(); math.Abs(got-12.0) > 0.25 {
		t.Fatalf("a twelve second stop past the interval reported %v", got)
	}
	if got := w.Slept(); got != 0 {
		t.Fatalf("the stop was reported twice, now %v", got)
	}
	first := AwakeSeconds()
	time.Sleep(20 * time.Millisecond)
	if advanced := AwakeSeconds() - first; advanced < 0.015 || advanced > 5 {
		t.Fatalf("awake clock advanced %.3fs over a 20ms sleep", advanced)
	}
	if windowsUnbiasedClock() == nil && awakeClock()() <= 0 {
		t.Fatal("monotonic awake clock did not start counting")
	}
}
