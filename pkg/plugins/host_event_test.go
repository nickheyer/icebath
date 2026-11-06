package plugins

import (
	"context"
	"strings"
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

// Go events reach a hosted plugin as threading.Event objects following the Go flag, and child events come back readable
func TestHostEventCrossesThePluginHost(t *testing.T) {
	installLiveServices(t)
	var sdkEvent any
	s := *installedServices()
	s.PluginSDK = func(_ context.Context, _, op string, _ []any, kwargs map[string]any) (any, error) {
		sdkEvent = kwargs["cancel_event"]
		return nil, nil
	}
	InstallServices(&s)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	dir := memoryProviderDir(t, h, "eventprobe", eventProbeProvider)
	obj, err := CollectMemoryProviderInHost(ctx, "eventprobe", dir, memoryModule("eventprobe"), false, nil)
	if err != nil || obj == nil {
		t.Fatalf("collect = %v %v", obj, err)
	}
	ev := NewHostEvent()
	got, err := obj.Call(ctx, "kind", []any{ev}, nil)
	if l, _ := got.([]any); err != nil || len(l) != 3 || l[0] != "ParentEvent" || l[1] != true || l[2] != false {
		t.Fatalf("kind before set = %v %v", got, err)
	}
	if got, err := obj.Call(ctx, "poll", []any{ev}, nil); err != nil || got.([]any)[0] != false || got.([]any)[1] != false || got.([]any)[2] != false {
		t.Fatalf("poll unset = %v %v", got, err)
	}
	woke := make(chan any, 1)
	go func() {
		v, err := obj.Call(ctx, "block", []any{ev}, nil)
		if err != nil {
			v = err
		}
		woke <- v
	}()
	select {
	case v := <-woke:
		t.Fatalf("wait returned %v before the Go side set the event", v)
	case <-time.After(300 * time.Millisecond):
	}
	ev.Set()
	select {
	case v := <-woke:
		if v != true {
			t.Fatalf("woken wait = %v", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a Go Set did not wake the child wait")
	}
	if got, err := obj.Call(ctx, "kind", []any{ev}, nil); err != nil || got.([]any)[2] != true {
		t.Fatalf("kind after set = %v %v", got, err)
	}
	if got, err := obj.Call(ctx, "block", []any{ev, 5.0}, nil); err != nil || got != true {
		t.Fatalf("wait on a set event = %v %v", got, err)
	}
	if _, err := obj.Call(ctx, "flip", []any{ev, false}, nil); err != nil || ev.IsSet() {
		t.Fatalf("child clear = %v, Go flag %v", err, ev.IsSet())
	}
	if _, err := obj.Call(ctx, "flip", []any{ev, true}, nil); err != nil || !ev.IsSet() {
		t.Fatalf("child set = %v, Go flag %v", err, ev.IsSet())
	}
	if back, err := obj.Call(ctx, "echo", []any{ev}, nil); err != nil || back != ev {
		t.Fatalf("echo = %v %v", back, err)
	}
	if _, err := obj.Call(ctx, "through_sdk", []any{ev}, nil); err != nil || sdkEvent != ev {
		t.Fatalf("sdk cancel_event = %#v %v", sdkEvent, err)
	}
	host, _ := m.pluginHost()
	host.stateMu.Lock()
	slots := 0
	for _, slot := range host.parents {
		if slot.event.Value() == ev {
			slots++
		}
	}
	host.stateMu.Unlock()
	if slots != 1 {
		t.Fatalf("event passed on every call holds %d parent slots", slots)
	}
	if _, err := obj.Call(ctx, "bad_wait", []any{ev}, nil); err == nil || !strings.Contains(err.Error(), "'>' not supported between instances of 'str' and 'int'") {
		t.Fatalf("wait with a str timeout = %v", err)
	}

	raw, err := obj.Call(ctx, "native", nil, nil)
	mine, ok := raw.(*HostEvent)
	if err != nil || !ok || mine.IsSet() || mine.Reason() != "" {
		t.Fatalf("native = %#v %v", raw, err)
	}
	if mine.Wait(ctx, 50*time.Millisecond) {
		t.Fatal("a native wait reported set before anything set it")
	}
	done := make(chan bool, 1)
	go func() { done <- mine.Wait(ctx, -1) }()
	time.Sleep(200 * time.Millisecond)
	if _, err := obj.Call(ctx, "native_set", []any{"fire ownership moved"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-done:
		if !v {
			t.Fatal("native wait woke unset")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a child set did not wake the Go wait")
	}
	if !mine.IsSet() || mine.Reason() != "fire ownership moved" {
		t.Fatalf("native after set = %v %q", mine.IsSet(), mine.Reason())
	}
	mine.clear()
	if st, err := obj.Call(ctx, "native_state", nil, nil); err != nil || st != false {
		t.Fatalf("native after Go clear = %v %v", st, err)
	}
	mine.Set()
	if st, err := obj.Call(ctx, "native_state", nil, nil); err != nil || st != true {
		t.Fatalf("native after Go set = %v %v", st, err)
	}
	if _, err := obj.Call(ctx, "kind", []any{mine}, nil); err != nil {
		t.Fatalf("native event sent back = %v", err)
	}

	host.Shutdown()
	if !mine.IsSet() {
		t.Fatal("a child event whose host is gone must read as set")
	}
}

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
