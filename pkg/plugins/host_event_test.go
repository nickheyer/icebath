package plugins

import (
	"context"
	"testing"
	"time"
)

const eventProbeProvider = `
import threading
from agent.memory_provider import MemoryProvider
from icebath_cli.plugin_sdk import parent_call

class EventProbe(MemoryProvider):
    @property
    def name(self): return "eventprobe"
    def is_available(self): return True
    def initialize(self, session_id, **kw): pass
    def get_tool_schemas(self): return []
    def kind(self, ev): return [type(ev).__name__, isinstance(ev, threading.Event), ev.is_set()]
    def block(self, ev, timeout=None): return ev.wait(timeout)
    def poll(self, ev): return [ev.wait(0.05), ev.wait(0), ev.wait(-1)]
    def flip(self, ev, on): ev.set() if on else ev.clear()
    def echo(self, ev): return ev
    def through_sdk(self, ev): return parent_call("cron.fire_claimed", {}, cancel_event=ev)
    def native(self):
        self.mine = threading.Event()
        return self.mine
    def native_set(self, reason):
        self.mine.reason = reason
        self.mine.set()
    def native_state(self): return self.mine.is_set()
    def bad_wait(self, ev): return ev.wait("soon")

def register(ctx):
    ctx.register_memory_provider(EventProbe())
`

// Python Event.wait timeouts map onto Go durations with the CPython refusals
func TestPyWaitTimeout(t *testing.T) {
	for _, c := range []struct {
		in   any
		want time.Duration
		err  string
	}{{nil, -1, ""}, {int64(2), 2 * time.Second, ""}, {0.25, 250 * time.Millisecond, ""}, {-3.0, 0, ""}, {true, time.Second, ""},
		{1e10, 0, "timestamp out of range for C PyTime_t"}, {"x", 0, "'>' not supported between instances of 'str' and 'int'"}} {
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
