package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var relayIdleDescriptor = map[string]any{"contract_version": 1, "platform": "discord", "label": "Discord", "max_message_length": 2000,
	"supports_draft_streaming": false, "supports_edit": true, "supports_threads": true, "markdown_dialect": "discord", "len_unit": "chars"}

// Loopback connector that answers hello with the descriptor and its pushes, acks going idle and records inbound acks
type relayIdleServer struct {
	url         string
	mu          sync.Mutex
	pushes      []map[string]any
	inboundAcks []string
	goingIdle   int
	connections int
	dropFirst   bool
	noIdleAck   bool
}

func newRelayIdleServer(t *testing.T, configure func(s *relayIdleServer)) *relayIdleServer {
	t.Helper()
	s := &relayIdleServer{}
	if configure != nil {
		configure(s)
	}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		s.mu.Lock()
		s.connections++
		conn := s.connections
		s.mu.Unlock()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var frame map[string]any
				json.Unmarshal([]byte(line), &frame)
				switch frame["type"] {
				case "hello":
					relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayIdleDescriptor})
					if s.dropFirst && conn == 1 {
						ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
						return
					}
					s.mu.Lock()
					pushes := slices.Clone(s.pushes)
					s.mu.Unlock()
					for _, f := range pushes {
						relayWSSend(ws, f)
					}
				case "going_idle":
					s.mu.Lock()
					s.goingIdle++
					s.mu.Unlock()
					if !s.noIdleAck {
						relayWSSend(ws, map[string]any{"type": "going_idle_ack"})
					}
				case "inbound_ack":
					s.mu.Lock()
					s.inboundAcks = append(s.inboundAcks, relayWireStr(frame["bufferId"]))
					s.mu.Unlock()
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return s
}

func (s *relayIdleServer) snapshot() (int, []string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections, slices.Clone(s.inboundAcks), s.goingIdle
}

// Inbound frame of a discord DM text, buffered when a buffer id is given
func relayIdleInbound(text, bufferID string) map[string]any {
	f := map[string]any{"type": "inbound", "event": map[string]any{"text": text, "message_type": "text",
		"source": map[string]any{"platform": "discord", "chat_id": "c1", "chat_type": "dm"}}}
	if bufferID != "" {
		f["bufferId"] = bufferID
	}
	return f
}

// Inbound handler recording each event text
func relayTextRecorder() (RelayInboundHandler, func() []string) {
	var mu sync.Mutex
	var seen []string
	return func(_ context.Context, e *MessageEvent) (RelayAdmission, error) {
			mu.Lock()
			seen = append(seen, e.Text)
			mu.Unlock()
			return nil, nil
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(seen)
		}
}

// Connects and handshakes the transport, disconnecting it at cleanup
func relayIdleConnect(t *testing.T, tr *WebSocketRelayTransport) {
	t.Helper()
	ctx := context.Background()
	if ok, err := tr.Connect(ctx); err != nil || !ok {
		t.Fatalf("connect %v err %v", ok, err)
	}
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { relayDisconnectNow(tr) })
}

func TestBufferedInboundIsAckedAfterHandler(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayIdleServer(t, func(s *relayIdleServer) {
		s.pushes = []map[string]any{relayIdleInbound("buffered", "buf-42"), relayIdleInbound("live", "")}
	})
	handler, seen := relayTextRecorder()
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{})
	tr.SetInboundHandler(handler)
	relayIdleConnect(t, tr)
	time.Sleep(100 * time.Millisecond)
	_, acks, _ := srv.snapshot()
	if got := seen(); !slices.Contains(got, "buffered") || !slices.Contains(got, "live") || !slices.Equal(acks, []string{"buf-42"}) {
		t.Fatalf("seen %v acks %v", got, acks)
	}
}

func TestReconnectRedialsAfterUnexpectedClose(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayIdleServer(t, func(s *relayIdleServer) { s.dropFirst = true })
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.05)})
	relayIdleConnect(t, tr)
	time.Sleep(200 * time.Millisecond)
	if conns, _, _ := srv.snapshot(); conns < 2 {
		t.Fatalf("connections %d", conns)
	}
}

func TestGoDormantRedialsOnWakeAndDrains(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayIdleServer(t, func(s *relayIdleServer) { s.pushes = []map[string]any{relayIdleInbound("while-asleep", "buf-wake-1")} })
	handler, seen := relayTextRecorder()
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(5.0)})
	tr.dormantRedialS = 0.05
	tr.SetInboundHandler(handler)
	relayIdleConnect(t, tr)
	before, _, _ := srv.snapshot()
	tr.GoDormant(context.Background(), 2)
	woke := func() bool {
		conns, acks, _ := srv.snapshot()
		return conns > before && slices.Contains(seen(), "while-asleep") && slices.Contains(acks, "buf-wake-1")
	}
	if !relayWaitUntil(woke) {
		conns, acks, _ := srv.snapshot()
		t.Fatalf("connections %d seen %v acks %v", conns, seen(), acks)
	}
	tr.mu.Lock()
	dormant := tr.dormant
	tr.mu.Unlock()
	if dormant {
		t.Fatal("a woken transport left the dormant cadence on")
	}
}

// Transport whose redial toggles raise
type relayThrowingRedial struct{ relayCapture }

func (*relayThrowingRedial) HoldRedial() {
	panic(&PyError{Kind: "RuntimeError", Msg: "transport refused the hold"})
}
func (*relayThrowingRedial) ReleaseRedial() {
	panic(&PyError{Kind: "RuntimeError", Msg: "transport refused the release"})
}

// Transport whose redial toggles record their calls
type relayWorkingRedial struct {
	relayCapture
	calls []string
}

func (w *relayWorkingRedial) HoldRedial()    { w.calls = append(w.calls, "hold") }
func (w *relayWorkingRedial) ReleaseRedial() { w.calls = append(w.calls, "release") }

func TestAdapterReportsFalseWhenTheTransportHoldFails(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	provisional := relayCapsDesc("discord", 2000, "chars")
	for _, transport := range []RelayTransportConn{&relayThrowingRedial{}, &relayCapture{}} {
		a := NewRelayAdapter(NewPlatformConfig(), provisional, transport)
		if a.HoldRedial() || a.ReleaseRedial() {
			t.Fatalf("%T reported a toggle that never took", transport)
		}
	}
	working := &relayWorkingRedial{}
	a := NewRelayAdapter(NewPlatformConfig(), provisional, working)
	if !a.HoldRedial() || !slices.Equal(working.calls, []string{"hold"}) {
		t.Fatalf("calls %v", working.calls)
	}
}

func TestGoDormantLeavesTheSocketOpenWhenTheAckIsMissed(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayIdleServer(t, func(s *relayIdleServer) { s.noIdleAck = true })
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.05)})
	relayIdleConnect(t, tr)
	if tr.GoDormant(context.Background(), 0.1) {
		t.Fatal("dormancy reported without the ack")
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.dormant || tr.conn == nil || tr.closing {
		t.Fatalf("dormant %v socket %v closing %v", tr.dormant, tr.conn != nil, tr.closing)
	}
}

func TestAdapterGoDormantDelegatesToTransport(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayIdleServer(t, nil)
	transport := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.05)})
	a := NewRelayAdapter(NewPlatformConfig(), relayCapsDesc("discord", 2000, "chars"), transport)
	ctx := context.Background()
	if ok, err := a.Connect(ctx, false); err != nil || !ok {
		t.Fatalf("connect %v err %v", ok, err)
	}
	defer a.Disconnect(ctx)
	ok, err := a.GoDormant(ctx)
	_, _, idles := srv.snapshot()
	transport.mu.Lock()
	closing, dormant := transport.closing, transport.dormant
	transport.mu.Unlock()
	if err != nil || !ok || idles != 1 || closing || !dormant {
		t.Fatalf("ok %v err %v idles %d closing %v dormant %v", ok, err, idles, closing, dormant)
	}
}
