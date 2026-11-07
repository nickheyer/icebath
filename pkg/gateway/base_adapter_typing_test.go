package gateway

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type typingAdapter struct {
	eaPlain
	mu         sync.Mutex
	sendTyping func(ctx context.Context, chatID string) error
	stops      [][2]any
	reactions  []string
}

func (a *typingAdapter) SendTyping(ctx context.Context, chatID string, _ map[string]any) error {
	return a.sendTyping(ctx, chatID)
}

func (a *typingAdapter) StopTyping(_ context.Context, chatID string, _ map[string]any) error {
	a.Base().mu.Lock()
	_, paused := a.Base().typingPaused[chatID]
	a.Base().mu.Unlock()
	a.mu.Lock()
	a.stops = append(a.stops, [2]any{chatID, paused})
	a.mu.Unlock()
	return nil
}

func (a *typingAdapter) AddReaction(_ context.Context, chatID, messageID, emoji string) error {
	a.reactions = append(a.reactions, "add:"+chatID+":"+messageID+":"+emoji)
	return nil
}

func (a *typingAdapter) RemoveReaction(_ context.Context, chatID, messageID string) error {
	a.reactions = append(a.reactions, "remove:"+chatID+":"+messageID)
	return nil
}

func newTypingAdapter() *typingAdapter {
	a := &typingAdapter{}
	a.BasePlatformAdapter = NewBasePlatformAdapter(NewPlatformConfig(), PlatformTelegram, a)
	return a
}

func TestSlowSendTypingDoesNotBlockCadence(t *testing.T) {
	a := newTypingAdapter()
	var starts int
	var mu sync.Mutex
	a.sendTyping = func(ctx context.Context, _ string) error {
		mu.Lock()
		starts++
		mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		return nil
	}
	stop := NewInterruptEvent()
	task := spawnAdapterTask(context.Background(), func(ctx context.Context) error {
		a.KeepTyping(ctx, "123", 1.0, nil, stop)
		return nil
	})
	time.Sleep(3 * time.Second)
	stop.Set()
	select {
	case <-task.done:
	case <-time.After(2 * time.Second):
		task.cancel()
		t.Fatal("keep typing did not exit within 2s of the stop event")
	}
	mu.Lock()
	defer mu.Unlock()
	if starts < 2 {
		t.Fatalf("expected at least 2 ticks, got %d", starts)
	}
}

func TestSendTypingExceptionDoesNotKillLoop(t *testing.T) {
	a := newTypingAdapter()
	var ticks int
	var mu sync.Mutex
	a.sendTyping = func(context.Context, string) error {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		if ticks == 1 {
			return errors.New("transient upstream error")
		}
		return nil
	}
	stop := NewInterruptEvent()
	task := spawnAdapterTask(context.Background(), func(ctx context.Context) error {
		a.KeepTyping(ctx, "789", 0.3, nil, stop)
		return nil
	})
	time.Sleep(time.Second)
	stop.Set()
	select {
	case <-task.done:
	case <-time.After(time.Second):
		t.Fatal("keep typing did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	if ticks < 2 {
		t.Fatalf("loop stopped after the first error, %d ticks", ticks)
	}
}

func TestStopTypingRefreshBlocksLateCancelTick(t *testing.T) {
	a := newTypingAdapter()
	var late []string
	a.sendTyping = func(_ context.Context, chatID string) error {
		late = append(late, chatID)
		return nil
	}
	started := make(chan struct{})
	task := spawnAdapterTask(context.Background(), func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		a.Base().mu.Lock()
		_, paused := a.Base().typingPaused["discord-chat"]
		a.Base().mu.Unlock()
		if !paused {
			_ = a.SendTyping(ctx, "discord-chat", nil)
		}
		return ctx.Err()
	})
	<-started
	a.stopTypingRefresh(context.Background(), "discord-chat", task, nil, 1.0, 2)
	if len(late) != 0 {
		t.Fatalf("late tick sent %v", late)
	}
	want := [][2]any{{"discord-chat", true}, {"discord-chat", true}}
	if !reflect.DeepEqual(a.stops, want) {
		t.Fatalf("stops %v", a.stops)
	}
	if _, paused := a.typingPaused["discord-chat"]; paused {
		t.Fatal("pause released after the stop")
	}
}

func firePostDelivery(t *testing.T, cb PostDeliveryCallback) {
	if cb == nil {
		t.Fatal("no callback")
	}
	_ = cb(context.Background())
}

func TestPostDeliveryCallbacksChainInOrder(t *testing.T) {
	for _, labels := range [][]string{{"A"}, {"A", "B"}, {"A", "B", "C"}} {
		a := newTypingAdapter()
		var fired []string
		for _, l := range labels {
			l := l
			a.RegisterPostDeliveryCallback("s", func(context.Context) error { fired = append(fired, l); return nil }, nil)
		}
		firePostDelivery(t, a.PopPostDeliveryCallback("s", nil))
		if !reflect.DeepEqual(fired, labels) {
			t.Fatalf("fired %v want %v", fired, labels)
		}
		if a.PopPostDeliveryCallback("s", nil) != nil {
			t.Fatal("slot popped once")
		}
	}
}

func TestAsyncCallbackInChainIsAwaited(t *testing.T) {
	a := newTypingAdapter()
	var fired []string
	a.RegisterPostDeliveryCallback("s", func(context.Context) error { fired = append(fired, "sync"); return nil }, nil)
	a.RegisterPostDeliveryCallback("s", func(context.Context) error {
		done := make(chan struct{})
		go func() { time.Sleep(time.Millisecond); close(done) }()
		<-done
		fired = append(fired, "async")
		return errors.New("isolated")
	}, nil)
	firePostDelivery(t, a.PopPostDeliveryCallback("s", nil))
	if !reflect.DeepEqual(fired, []string{"sync", "async"}) {
		t.Fatalf("fired %v", fired)
	}
}

func TestPostDeliveryGenerationOwnership(t *testing.T) {
	a := newTypingAdapter()
	var fired []string
	g1, g2 := 1, 2
	a.RegisterPostDeliveryCallback("s", func(context.Context) error { fired = append(fired, "g2"); return nil }, &g2)
	a.RegisterPostDeliveryCallback("s", func(context.Context) error { fired = append(fired, "stale"); return nil }, &g1)
	if a.PopPostDeliveryCallback("s", &g1) != nil {
		t.Fatal("a foreign generation cannot pop the slot")
	}
	firePostDelivery(t, a.PopPostDeliveryCallback("s", &g2))
	if !reflect.DeepEqual(fired, []string{"g2"}) {
		t.Fatalf("fired %v", fired)
	}
	RunPostDeliveryCallback(context.Background(), func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, 0.05)
	RunPostDeliveryCallback(context.Background(), nil, 1)
}

func TestOnProcessingCompleteSwapsReaction(t *testing.T) {
	a := newTypingAdapter()
	mid := "m1"
	ev := &MessageEvent{MessageID: &mid, Source: &SessionSource{ChatID: "c1"}}
	_ = a.OnProcessingComplete(context.Background(), ev, ProcessingSuccess)
	if len(a.reactions) != 0 {
		t.Fatal("no emoji configured leaves the message unreacted")
	}
	a.Traits.OkEmoji, a.Traits.FailEmoji = "ok", "fail"
	_ = a.OnProcessingComplete(context.Background(), ev, ProcessingFailure)
	_ = a.OnProcessingComplete(context.Background(), ev, ProcessingCancelled)
	want := []string{"remove:c1:m1", "add:c1:m1:fail", "remove:c1:m1"}
	if !reflect.DeepEqual(a.reactions, want) {
		t.Fatalf("reactions %v", a.reactions)
	}
	logs := captureLogs(t)
	a.runProcessingHook(context.Background(), "on_processing_start", func(context.Context) error { return errors.New("boom") })
	if !reflect.DeepEqual(logs.Len() > 0, true) {
		t.Fatal("hook failure is logged")
	}
}

func runStallLoop(t *testing.T, a *typingAdapter, chatID string) []string {
	var ticks []string
	var mu sync.Mutex
	a.sendTyping = func(_ context.Context, id string) error {
		mu.Lock()
		ticks = append(ticks, id)
		mu.Unlock()
		return nil
	}
	stop := NewInterruptEvent()
	task := spawnAdapterTask(context.Background(), func(ctx context.Context) error {
		a.KeepTyping(ctx, chatID, 0.2, nil, stop)
		return nil
	})
	time.Sleep(800 * time.Millisecond)
	stop.Set()
	select {
	case <-task.done:
	case <-time.After(time.Second):
		t.Fatal("keep typing did not exit")
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), ticks...)
}

func TestPausedBeforeDeliveryStallSuppressesRefresh(t *testing.T) {
	a := newTypingAdapter()
	a.PauseTypingForChat("123")
	if ticks := runStallLoop(t, a, "123"); len(ticks) != 0 {
		t.Fatalf("typing refreshed across a pause: %v", ticks)
	}
	a.stopTypingRefresh(context.Background(), "123", nil, nil, 0.5, 1)
	if _, paused := a.typingPaused["123"]; paused {
		t.Fatal("pause released")
	}
}

func TestUnpausedStallStillTicks(t *testing.T) {
	if ticks := runStallLoop(t, newTypingAdapter(), "456"); len(ticks) < 2 {
		t.Fatalf("expected at least 2 ticks, got %v", ticks)
	}
}
