package sleepclock

import (
	"testing"
	"time"
)

func TestAwakeSecondsAdvancesMonotonically(t *testing.T) {
	first := AwakeSeconds()
	time.Sleep(20 * time.Millisecond)
	second := AwakeSeconds()
	if second-first < 0.015 || second-first > 5 {
		t.Fatalf("awake clock advanced %.3fs over a 20ms sleep", second-first)
	}
}
