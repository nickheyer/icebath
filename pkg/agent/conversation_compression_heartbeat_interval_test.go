package agent

import (
	"testing"
	"time"
)

func TestHeartbeatIntervalFollowsPythonFloatOfIntervalOr60(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{"0", 0}, {" 5 ", 5}, {"1_0", 10}, {"inf", 60}, {"nan", 60}, {true, 1}, {false, 60},
		{2, 2}, {0.0, 60}, {[]any{}, 60}, {[]any{1}, 60}, {"abc", 60},
	}
	for _, c := range cases {
		if got := heartbeatSeconds(c.in); got != c.want {
			t.Fatalf("heartbeatSeconds(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
	h := newCompressionActivityHeartbeat(&AIAgent{}, "0", false, nil)
	if h.interval != 100*time.Millisecond {
		t.Fatalf("interval %v, want the 0.1 second floor", h.interval)
	}
}
