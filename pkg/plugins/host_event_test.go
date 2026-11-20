package plugins

import (
	"context"
	"testing"
	"time"
)

// Python Event.wait timeouts map onto Go durations with the CPython refusals
func TestPyWaitTimeout(t *testing.T) {
	for _, c := range []struct {
		in   any
		want time.Duration
		err  string
	}{{nil, -1, ""}, {int64(2), 2 * time.Second, ""}, {0.25, 250 * time.Millisecond, ""}, {-3.0, 0, ""}, {true, time.Second, ""},
		{1e10, 0, "event wait timeout of 10000000000 seconds is too large"}, {"x", 0, "event wait timeout is text where a number was expected"}} {
		got, err := pyWaitTimeout(c.in)
		if (err == nil) != (c.err == "") || (err != nil && err.Error() != c.err) || (err == nil && got != c.want) {
			t.Fatalf("pyWaitTimeout(%v) = %v %v", c.in, got, err)
		}
	}
	ev := NewHostEvent()
	if ev.Wait(context.Background(), 0) || ev.IsSet() {
		t.Fatal("fresh event reads set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ev.Wait(ctx, -1) {
		t.Fatal("cancelled wait reads set")
	}
	ev.Set()
	ev.Set()
	if !ev.Wait(context.Background(), -1) || !ev.IsSet() {
		t.Fatal("set event reads unset")
	}
	ev.clear()
	if ev.IsSet() || ev.Wait(context.Background(), time.Millisecond) {
		t.Fatal("cleared event reads set")
	}
}
