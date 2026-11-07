package telegram

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type fakeHeldHost struct {
	*gateway.BasePlatformAdapter
	held     *HeldInbound
	mu       sync.Mutex
	fatal    bool
	drop     bool
	accepted int
	handled  []string
	handle   func(ctx context.Context, event *gateway.MessageEvent) error
}

func newFakeHeldHost() *fakeHeldHost {
	h := &fakeHeldHost{}
	h.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.Platform("telegram"), h)
	h.held = NewHeldInbound(h)
	return h
}

func (h *fakeHeldHost) Connect(context.Context, bool) (bool, error) { return true, nil }
func (h *fakeHeldHost) Disconnect(context.Context) error            { return nil }
func (h *fakeHeldHost) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{}, nil
}
func (h *fakeHeldHost) GetChatInfo(context.Context, string) (map[string]any, error) { return nil, nil }

func (h *fakeHeldHost) set(fatal, drop bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fatal, h.drop = fatal, drop
}

func (h *fakeHeldHost) IsPermanentFatal() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fatal
}

func (h *fakeHeldHost) ShouldDropDelayedDelivery() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.drop
}

func (h *fakeHeldHost) AcceptUpdate(context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.accepted++
}

func (h *fakeHeldHost) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	h.mu.Lock()
	h.handled = append(h.handled, event.Text)
	handle := h.handle
	h.mu.Unlock()
	if handle != nil {
		return handle(ctx, event)
	}
	return nil
}

func (h *fakeHeldHost) handledTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.handled...)
}

func heldEvent(text string) *gateway.MessageEvent {
	return &gateway.MessageEvent{Text: text, Source: &gateway.SessionSource{ChatID: "12345"}}
}

func heldTexts(h *HeldInbound) []string {
	var out []string
	for _, e := range h.Events() {
		out = append(out, e.Text)
	}
	return out
}

func transportRef(source *gateway.SessionSource) gateway.PlatformAdapter {
	f := reflect.ValueOf(source).Elem().FieldByName("transportAdapterRef")
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(gateway.PlatformAdapter)
}

func waitTask(t *testing.T, task *gateway.AdapterTask) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for task != nil && !task.Done() {
		if time.Now().After(deadline) {
			t.Fatal("drain task did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not hold")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHoldDedupesSameEventObject(t *testing.T) {
	h := newFakeHeldHost()
	h.set(false, true)
	event := heldEvent("once")
	h.held.HoldInboundEvent(context.Background(), event, "a", true)
	h.held.HoldInboundEvent(context.Background(), event, "b", true)
	if n := len(h.held.Events()); n != 1 || h.accepted != 1 {
		t.Fatalf("held %d accepted %d", n, h.accepted)
	}
}

func TestHeldQueueCapDropsOldest(t *testing.T) {
	h := newFakeHeldHost()
	h.set(false, true)
	h.held.Max = 2
	for _, text := range []string{"first", "second", "third"} {
		h.held.HoldInboundEvent(context.Background(), heldEvent(text), "text-enqueue", true)
	}
	if got := heldTexts(h.held); !slices.Equal(got, []string{"second", "third"}) {
		t.Fatalf("held %v", got)
	}
	if h.held.RedispatchTask() != nil {
		t.Fatal("a disconnected hold must wait for reconnect")
	}
}

func TestHeldQueueZeroCapUsesClassDefault(t *testing.T) {
	h := newFakeHeldHost()
	h.set(false, true)
	h.held.Max = 0
	for range HeldInboundMax + 3 {
		h.held.HoldInboundEvent(context.Background(), heldEvent("x"), "a", false)
	}
	if n := len(h.held.Events()); n != HeldInboundMax {
		t.Fatalf("held %d", n)
	}
}

func TestPermanentFatalHoldDiscards(t *testing.T) {
	h := newFakeHeldHost()
	h.set(true, true)
	h.held.HoldInboundEvent(context.Background(), heldEvent("doomed"), "text-enqueue", true)
	if n := len(h.held.Events()); n != 0 || h.accepted != 0 {
		t.Fatalf("held %d accepted %d", n, h.accepted)
	}
}

func TestRedispatchAbortsCleanlyIfDisconnectReturns(t *testing.T) {
	h := newFakeHeldHost()
	h.held.SetEvents([]*gateway.MessageEvent{heldEvent("a"), heldEvent("b"), heldEvent("c")})
	h.handle = func(context.Context, *gateway.MessageEvent) error {
		h.set(false, true)
		return nil
	}
	if err := h.held.redispatchHeldInbound(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := h.handledTexts(); len(got) != 1 {
		t.Fatalf("handled %v", got)
	}
	if got := heldTexts(h.held); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("held %v", got)
	}
}

func TestRedispatchUnderPermanentFatalDiscardsHeld(t *testing.T) {
	h := newFakeHeldHost()
	h.held.SetEvents([]*gateway.MessageEvent{heldEvent("doomed")})
	h.set(true, true)
	if err := h.held.redispatchHeldInbound(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(h.held.Events()) != 0 || len(h.handledTexts()) != 0 {
		t.Fatal("permanent fatal must discard without dispatch")
	}
}

func TestConnectedHoldSchedulesRedispatch(t *testing.T) {
	h := newFakeHeldHost()
	h.held.HoldInboundEvent(context.Background(), heldEvent("orphan-without-drain"), "text-flush-cancelled", true)
	drain := h.held.RedispatchTask()
	if drain == nil {
		t.Fatal("connected hold must schedule a drain")
	}
	waitTask(t, drain)
	if got := h.handledTexts(); !slices.Equal(got, []string{"orphan-without-drain"}) {
		t.Fatalf("handled %v", got)
	}
	if len(h.held.Events()) != 0 {
		t.Fatal("drain must empty the queue")
	}
}

func TestRedispatchExceptionReholdsCurrentAndRemainder(t *testing.T) {
	h := newFakeHeldHost()
	h.held.SetEvents([]*gateway.MessageEvent{heldEvent("boom"), heldEvent("after")})
	h.handle = func(_ context.Context, event *gateway.MessageEvent) error {
		if event.Text == "boom" {
			return errors.New("dispatch failed")
		}
		return nil
	}
	if err := h.held.redispatchHeldInbound(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if got := heldTexts(h.held); !slices.Equal(got, []string{"boom", "after"}) {
		t.Fatalf("held %v", got)
	}
	if h.held.RedispatchTask() != nil {
		t.Fatal("a failed drain must not reschedule itself")
	}
}

func TestRedispatchCancelledReholdsCurrentAndRemainder(t *testing.T) {
	h := newFakeHeldHost()
	entered := make(chan struct{})
	h.handle = func(ctx context.Context, _ *gateway.MessageEvent) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	h.held.SetEvents([]*gateway.MessageEvent{heldEvent("a"), heldEvent("b")})
	h.held.ScheduleHeldInboundRedispatch(context.Background())
	drain := h.held.RedispatchTask()
	<-entered
	h.set(false, true)
	gateway.CancelTask(context.Background(), drain)
	if got := heldTexts(h.held); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("held %v", got)
	}
	if h.held.RedispatchTask() != drain {
		t.Fatal("a teardown cancel must not schedule another drain")
	}
}

func TestScheduleSkipsWhileAnotherDrainRunsAndFollowsUpAfter(t *testing.T) {
	h := newFakeHeldHost()
	release := make(chan struct{})
	h.handle = func(_ context.Context, event *gateway.MessageEvent) error {
		if event.Text == "first" {
			<-release
		}
		return nil
	}
	h.held.HoldInboundEvent(context.Background(), heldEvent("first"), "a", true)
	drain := h.held.RedispatchTask()
	waitFor(t, func() bool { return len(h.handledTexts()) == 1 })
	h.held.HoldInboundEvent(context.Background(), heldEvent("late"), "b", true)
	if h.held.RedispatchTask() != drain {
		t.Fatal("a running drain must not be replaced from outside it")
	}
	close(release)
	waitFor(t, func() bool { return slices.Equal(h.handledTexts(), []string{"first", "late"}) })
	waitTask(t, h.held.RedispatchTask())
	if len(h.held.Events()) != 0 {
		t.Fatal("follow up drain must empty the queue")
	}
}

func TestHoldInsideDrainStartsItsOwnPass(t *testing.T) {
	h := newFakeHeldHost()
	h.handle = func(ctx context.Context, event *gateway.MessageEvent) error {
		if event.Text == "first" {
			h.held.HoldInboundEvent(ctx, heldEvent("nested"), "inner", true)
		}
		return nil
	}
	h.held.HoldInboundEvent(context.Background(), heldEvent("first"), "a", true)
	waitFor(t, func() bool { return slices.Equal(h.handledTexts(), []string{"first", "nested"}) })
}

func TestAdoptHeldInboundMovesQueueAndForwardsLaterHolds(t *testing.T) {
	old, replacement := newFakeHeldHost(), newFakeHeldHost()
	old.set(false, true)
	replacement.set(false, true)
	first, second := heldEvent("first"), heldEvent("second")
	old.held.SetEvents([]*gateway.MessageEvent{first, second})
	replacement.held.AdoptHeldInbound(context.Background(), old.held)
	if got := heldTexts(replacement.held); !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("adopted %v", got)
	}
	if len(old.held.Events()) != 0 || old.held.Successor() != replacement.held {
		t.Fatal("predecessor must hand its queue over and link the successor")
	}
	if transportRef(first.Source) != replacement || transportRef(second.Source) != replacement {
		t.Fatal("adopted events must rebind their transport ref")
	}
	salvage := heldEvent("salvage")
	old.held.HoldInboundEvent(context.Background(), salvage, "teardown-salvage", true)
	if got := heldTexts(replacement.held); !slices.Equal(got, []string{"first", "second", "salvage"}) {
		t.Fatalf("forwarded %v", got)
	}
	if old.accepted != 1 || len(old.held.Events()) != 0 || transportRef(salvage.Source) != replacement {
		t.Fatal("a retired hold must accept its claim and forward to the successor")
	}
	old.held.AdoptHeldInbound(context.Background(), replacement.held)
	if old.held.Successor() != nil || replacement.held.Successor() != old.held {
		t.Fatal("adopting back must break the reverse link")
	}
	if got := heldTexts(old.held); !slices.Equal(got, []string{"first", "second", "salvage"}) {
		t.Fatalf("returned %v", got)
	}
}

func TestAdoptHeldInboundDrainsWhenConnected(t *testing.T) {
	old, replacement := newFakeHeldHost(), newFakeHeldHost()
	old.set(false, true)
	old.held.SetEvents([]*gateway.MessageEvent{heldEvent("held")})
	replacement.held.AdoptHeldInbound(context.Background(), old.held)
	waitTask(t, replacement.held.RedispatchTask())
	if got := replacement.handledTexts(); !slices.Equal(got, []string{"held"}) || len(old.handledTexts()) != 0 {
		t.Fatalf("replacement handled %v", got)
	}
}
