package agent

import (
	"math"
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

func TestCompressionLockTTLFollowsPythonFloatOfTTLOr300(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{nil, 300}, {"0", 0}, {"", 300}, {0, 300}, {0.0, 300}, {false, 300}, {true, 1}, {" 5 ", 5},
		{"1_0", 10}, {"abc", 300}, {[]any{1}, 300}, {[]any{}, 300}, {45, 45}, {int64(7), 7}, {12.5, 12.5},
	}
	for _, c := range cases {
		if got := compressionLockTTL(c.in); got != c.want {
			t.Fatalf("compressionLockTTL(%#v) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := compressionLockTTL("inf"); !math.IsInf(got, 1) {
		t.Fatalf("compressionLockTTL(\"inf\") = %v, want +Inf", got)
	}
	if got := compressionLockTTL("nan"); !math.IsNaN(got) {
		t.Fatalf("compressionLockTTL(\"nan\") = %v, want NaN", got)
	}
}
