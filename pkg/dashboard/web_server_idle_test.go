package dashboard

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWSSessionsAreCountedAtTheASGIBoundaryForAnyRoute(t *testing.T) {
	app := NewFastAPI("t", "1", nil, nil)
	app.AddAPIWebSocketRoute("/api/anything", &WSEndpoint{Name: "anything", Handle: func(ctx context.Context, c *Call) error {
		if err := c.WebSocket.Accept(ctx, nil, nil); err != nil {
			return err
		}
		if _, err := c.WebSocket.ReceiveText(ctx); err != nil {
			return err
		}
		return c.WebSocket.Close(ctx, 1000, "")
	}})
	app.AddAPIWebSocketRoute("/api/refused", &WSEndpoint{Name: "refused", Handle: func(ctx context.Context, c *Call) error {
		return c.WebSocket.Close(ctx, 4401, "")
	}})
	tracker := NewIdleClientTracker(func() float64 { return 0 })
	srv := NewUvicornServer(&UvicornConfig{App: WrapASGIWithWSTracking(app.ASGI, tracker), Host: "127.0.0.1", Port: 0,
		TimeoutGracefulShutdown: 3 * time.Second})
	if err := srv.Startup(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())
	url := "ws://" + srv.Addr().String()
	waitCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for tracker.LiveCount() != want {
			if time.Now().After(deadline) {
				t.Fatalf("live %d, want %d", tracker.LiveCount(), want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	a, _, err := websocket.DefaultDialer.Dial(url+"/api/anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := websocket.DefaultDialer.Dial(url+"/api/anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitCount(2)
	b.WriteMessage(websocket.TextMessage, []byte("bye"))
	b.ReadMessage()
	waitCount(1)
	a.WriteMessage(websocket.TextMessage, []byte("bye"))
	a.ReadMessage()
	waitCount(0)
	if _, _, err := websocket.DefaultDialer.Dial(url+"/api/refused", nil); err == nil {
		t.Fatal("refused upgrade must fail")
	}
	waitCount(0)
}

func TestExitOnlyAfterGraceWithNoClientAndNoRunningTurn(t *testing.T) {
	var mu sync.Mutex
	clock := 0.0
	tracker := NewIdleClientTracker(func() float64 { mu.Lock(); defer mu.Unlock(); return clock })
	set := func(v float64) { mu.Lock(); clock = v; mu.Unlock() }
	idle := func() (bool, bool) { return false, true }
	busy := func() (bool, bool) { return true, true }
	unknown := func() (bool, bool) { return false, false }
	grace := 900.0
	if ShouldExitIdle(tracker, grace, idle) {
		t.Fatal("just started")
	}
	set(901)
	if !ShouldExitIdle(tracker, grace, idle) || ShouldExitIdle(tracker, grace, busy) || ShouldExitIdle(tracker, grace, unknown) {
		t.Fatal("grace passed verdicts")
	}
	tracker.OnOpen()
	set(5000)
	if ShouldExitIdle(tracker, grace, idle) {
		t.Fatal("a client is connected")
	}
	tracker.OnClose()
	if ShouldExitIdle(tracker, grace, idle) {
		t.Fatal("grace restarts on close")
	}
	set(5000 + grace + 1)
	if !ShouldExitIdle(tracker, grace, idle) {
		t.Fatal("grace elapsed after close")
	}
}

func TestIdleProofIsTrueOnlyWhenEveryLedgerIsProvablyEmpty(t *testing.T) {
	turn := func(busy bool, detail string, ok bool) TurnProbe {
		return func() (bool, string, bool) { return busy, detail, ok }
	}
	input := func(n int, ok bool) func() (int, bool) { return func() (int, bool) { return n, ok } }
	for _, c := range []struct {
		got, want map[string]any
	}{
		{IdleProof(turn(false, "", true), input(0, true)), map[string]any{"idle": true, "reason": nil}},
		{IdleProof(turn(true, "", true), input(0, true)), map[string]any{"idle": false, "reason": "turn_in_flight", "detail": nil}},
		{IdleProof(turn(false, "", true), input(1, true)), map[string]any{"idle": false, "reason": "awaiting_human_input"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Fatalf("got %v want %v", c.got, c.want)
		}
	}
	if d := IdleProof(turn(true, "session:abc", true), input(0, true))["detail"]; d != "session:abc" {
		t.Fatalf("detail %v", d)
	}
	if IdleProof(turn(false, "", false), input(0, true))["idle"] != nil || IdleProof(turn(false, "", true), input(0, false))["idle"] != nil {
		t.Fatal("indeterminate probes must not report idle")
	}
}
