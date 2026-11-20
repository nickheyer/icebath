package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Server answering hello with a descriptor and dropping the socket once signalled, with a close code or abruptly
func relayDroppingServer(t *testing.T, code int) (*relayWSServer, chan struct{}) {
	drop := make(chan struct{})
	srv := newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, _ int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		go func() {
			<-drop
			if code != 0 {
				ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
				return
			}
			ws.UnderlyingConn().Close()
		}()
		return true
	})
	return srv, drop
}

// Connected transport whose reader has handshaken with the dropping server
func relayConnectedTransport(t *testing.T, url string, o RelayTransportOptions) *WebSocketRelayTransport {
	t.Helper()
	tr := NewWebSocketRelayTransport(url, "discord", "bot1", o)
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Disconnect(context.Background(), relayFloatPtr(0)) })
	return tr
}

// Drops the socket and waits for the reader to unwind
func relayDropAndWait(t *testing.T, tr *WebSocketRelayTransport, drop chan struct{}) {
	t.Helper()
	tr.mu.Lock()
	reader := tr.reader
	tr.mu.Unlock()
	close(drop)
	select {
	case <-reader.done:
	case <-time.After(2 * time.Second):
		t.Fatal("reader never exited")
	}
}

func (t *WebSocketRelayTransport) pendingCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

func TestReadLoopExitFailsPendingFuturesPromptly(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv, drop := newRelayWSServerSilent(t)
	tr := relayConnectedTransport(t, srv.url, RelayTransportOptions{OutboundTimeoutS: relayFloatPtr(30.0)})
	done := make(chan map[string]any, 1)
	go func() {
		r, _ := tr.SendOutbound(context.Background(), map[string]any{"op": "send_message", "text": "hi"}, "")
		done <- r
	}()
	if !relayWaitUntil(func() bool { return tr.pendingCount() > 0 }) {
		t.Fatal("outbound future never registered")
	}
	close(drop)
	select {
	case r := <-done:
		if len(r) != 2 || r["success"] != false || r["error"] != "relay transport connection lost" {
			t.Fatalf("result %v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending send not failed promptly")
	}
	if tr.pendingCount() != 0 {
		t.Fatal("pending left behind")
	}
}

// Server that answers hello but never acknowledges outbound frames, dropping abruptly once signalled
func newRelayWSServerSilent(t *testing.T) (*relayWSServer, chan struct{}) {
	drop := make(chan struct{})
	srv := newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, _ int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		go func() {
			<-drop
			ws.UnderlyingConn().Close()
		}()
		return true
	})
	srv.silentOutbound = true
	return srv, drop
}

func TestSendDuringRedialWindowFailsFast(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv, drop := relayDroppingServer(t, 0)
	tr := relayConnectedTransport(t, srv.url, RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(60.0), OutboundTimeoutS: relayFloatPtr(30.0)})
	relayDropAndWait(t, tr, drop)
	tr.mu.Lock()
	sup := tr.supervisor
	tr.mu.Unlock()
	if sup == nil || sup.finished() {
		t.Fatal("reader exit must arm the reconnect supervisor")
	}
	start := time.Now()
	r, err := tr.SendOutbound(context.Background(), map[string]any{"op": "send_message", "text": "hi"}, "")
	if err != nil || r["success"] != false || time.Since(start) > time.Second || tr.pendingCount() != 0 {
		t.Fatalf("result %v err %v", r, err)
	}
}

func TestReadLoopWithoutSocketStillFailsPending(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	tr := NewWebSocketRelayTransport("ws://unused", "discord", "bot1", RelayTransportOptions{OutboundTimeoutS: relayFloatPtr(30.0)})
	p := &relayPending{fut: newRelayFuture[map[string]any]()}
	tr.pending["rid"] = p
	tr.readLoop(context.Background(), nil, 0)
	if !p.fut.isDone() || len(p.fut.val) != 2 || p.fut.val["success"] != false || p.fut.val["error"] != "relay transport connection lost" || tr.pendingCount() != 0 {
		t.Fatalf("future %v", p.fut.val)
	}
}

func TestSendAfterTerminal4401RevocationFailsFast(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv, drop := relayDroppingServer(t, 4401)
	tr := relayConnectedTransport(t, srv.url, RelayTransportOptions{Reconnect: true, OutboundTimeoutS: relayFloatPtr(30.0)})
	tr.mu.Lock()
	gen := tr.dialGeneration
	tr.authRetryGeneration = &gen
	tr.mu.Unlock()
	relayDropAndWait(t, tr, drop)
	_, sup, _, connected := tr.testState()
	if !tr.AuthRevoked() || sup || connected {
		t.Fatalf("revoked %v supervisor %v connected %v", tr.AuthRevoked(), sup, connected)
	}
	start := time.Now()
	r, _ := tr.SendOutbound(context.Background(), map[string]any{"op": "send_message", "text": "hi"}, "")
	if r["success"] != false || time.Since(start) > 2*time.Second || tr.pendingCount() != 0 {
		t.Fatalf("result %v", r)
	}
}

func TestSendAfterDropWithReconnectDisabledFailsFast(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv, drop := relayDroppingServer(t, 0)
	tr := relayConnectedTransport(t, srv.url, RelayTransportOptions{OutboundTimeoutS: relayFloatPtr(30.0)})
	relayDropAndWait(t, tr, drop)
	if _, _, _, connected := tr.testState(); connected {
		t.Fatal("dead socket handle must not survive the reader")
	}
	start := time.Now()
	r, _ := tr.SendOutbound(context.Background(), map[string]any{"op": "send_message", "text": "hi"}, "")
	if r["success"] != false || time.Since(start) > 2*time.Second || tr.pendingCount() != 0 {
		t.Fatalf("result %v", r)
	}
}

// Server answering every hello with a descriptor, counting dials
func relayCountingServer(t *testing.T) *relayWSServer {
	return newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, _ int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		return true
	})
}

func TestRedialHoldParksTheSupervisorUntilReleased(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayCountingServer(t)
	tr := NewWebSocketRelayTransport(srv.url, "discord", "bot1", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.01)})
	tr.dormantRedialS = 0.01
	tr.HoldRedial()
	sup := startRelayTask(context.Background(), tr.reconnectLoop)
	defer func() {
		tr.Disconnect(context.Background(), relayFloatPtr(0))
		sup.cancelBounded()
	}()
	time.Sleep(150 * time.Millisecond)
	if srv.dialCount() != 0 {
		t.Fatal("supervisor re-dialled while a brokered suspend was in flight")
	}
	tr.ReleaseRedial()
	if !relayWaitUntil(func() bool { return srv.dialCount() > 0 }) || srv.dialCount() != 1 {
		t.Fatalf("dials %d", srv.dialCount())
	}
}

func TestRedialHoldExpiresSoALostSuspendCannotStrandUs(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayCountingServer(t)
	tr := NewWebSocketRelayTransport(srv.url, "discord", "bot1", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.01)})
	tr.dormantRedialS = 0.01
	tr.redialHoldMaxS = 0.05
	tr.HoldRedial()
	sup := startRelayTask(context.Background(), tr.reconnectLoop)
	defer func() {
		tr.Disconnect(context.Background(), relayFloatPtr(0))
		sup.cancelBounded()
	}()
	if !relayWaitUntil(func() bool { return srv.dialCount() > 0 }) || srv.dialCount() != 1 {
		t.Fatalf("dials %d", srv.dialCount())
	}
	tr.mu.Lock()
	held := tr.redialHeld
	tr.mu.Unlock()
	if held {
		t.Fatal("hold survived its expiry")
	}
}
