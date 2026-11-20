package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var relayWSDescriptor = map[string]any{"contract_version": 1, "platform": "discord", "label": "Discord", "max_message_length": 2000,
	"supports_draft_streaming": false, "supports_edit": true, "supports_threads": true, "markdown_dialect": "discord", "len_unit": "chars"}

// Loopback websocket connector whose per dial behaviour is the handler
type relayWSServer struct {
	srv            *httptest.Server
	url            string
	mu             sync.Mutex
	dials          int
	received       []map[string]any
	silentOutbound bool
}

func newRelayWSServer(t *testing.T, onHello func(s *relayWSServer, ws *websocket.Conn, dial int) bool) *relayWSServer {
	t.Helper()
	s := &relayWSServer{}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		s.mu.Lock()
		s.dials++
		dial := s.dials
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
				s.mu.Lock()
				s.received = append(s.received, frame)
				s.mu.Unlock()
				switch frame["type"] {
				case "hello":
					if !onHello(s, ws, dial) {
						return
					}
				case "outbound":
					s.mu.Lock()
					silent := s.silentOutbound
					s.mu.Unlock()
					if silent {
						continue
					}
					action, _ := frame["action"].(map[string]any)
					relayWSSend(ws, map[string]any{"type": "outbound_result", "requestId": frame["requestId"],
						"result": map[string]any{"success": true, "message_id": "srv-" + relayWireStr(action["op"])}})
				}
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	s.url = "ws" + strings.TrimPrefix(s.srv.URL, "http")
	return s
}

func relayWSSend(ws *websocket.Conn, frame map[string]any) {
	out, _ := json.Marshal(frame)
	ws.WriteMessage(websocket.TextMessage, append(out, '\n'))
}

// Close frame with the code and reason, then the closing handshake reply awaited
func relayWSClose(ws *websocket.Conn, code int, reason string) {
	ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	ws.SetReadDeadline(time.Now().Add(time.Second))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
	}
}

func (s *relayWSServer) dialCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

// Polls the predicate every 20ms until it holds or two seconds pass
func relayWaitUntil(pred func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return pred()
}

func relayFloatPtr(f float64) *float64 { return &f }

// Transport state read under its lock for assertions
func (t *WebSocketRelayTransport) testState() (descriptor bool, supervisor bool, readerLive bool, connected bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.descriptor != nil, t.supervisor != nil, t.reader != nil && !t.reader.finished(), t.conn != nil
}

func TestHandshakeNegotiatesDescriptor(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, _ int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		return true
	})
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{})
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	desc, err := tr.Handshake(ctx)
	if err != nil || desc.Platform != "discord" || desc.MaxMessageLength != 2000 {
		t.Fatalf("descriptor %+v err %v", desc, err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.received[0]["type"] != "hello" || srv.received[0]["platform"] != "discord" || srv.received[0]["botId"] != "appShared" {
		t.Fatalf("hello %v", srv.received[0])
	}
}

func TestInboundFrameReachesHandler(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, _ int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		relayWSSend(ws, map[string]any{"type": "inbound", "bufferId": "buf-1", "event": map[string]any{
			"text": "hello from connector", "message_type": "text",
			"source": map[string]any{"platform": "discord", "chat_id": "chan1", "chat_type": "group", "scope_id": "guildA"}}})
		return true
	})
	var mu sync.Mutex
	var received []*MessageEvent
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{})
	tr.SetInboundHandler(func(_ context.Context, ev *MessageEvent) (RelayAdmission, error) {
		mu.Lock()
		received = append(received, ev)
		mu.Unlock()
		return nil, nil
	})
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || received[0].Text != "hello from connector" || received[0].Source.ScopeID == nil || *received[0].Source.ScopeID != "guildA" {
		t.Fatalf("received %v", received)
	}
}

// Server sending a descriptor then closing 4401 on the dials the predicate picks
func relayExpiredTokenServer(t *testing.T, closeOn func(dial int) bool, reason string) *relayWSServer {
	return newRelayWSServer(t, func(_ *relayWSServer, ws *websocket.Conn, dial int) bool {
		relayWSSend(ws, map[string]any{"type": "descriptor", "descriptor": relayWSDescriptor})
		if closeOn(dial) {
			time.Sleep(50 * time.Millisecond)
			relayWSClose(ws, 4401, reason)
			return false
		}
		return true
	})
}

// Transport with upgrade credentials and a slow backoff the fresh token retry must bypass
func relayAuthTransport(url string) *WebSocketRelayTransport {
	return NewWebSocketRelayTransport(url, "discord", "appShared", RelayTransportOptions{GatewayID: "gw-x", UpgradeSecret: "secret-x",
		Reconnect: true, ReconnectBackoffS: relayFloatPtr(5.0)})
}

func Test4401AfterHandshakeIsTerminalNoReconnect(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayExpiredTokenServer(t, func(int) bool { return true }, "unauthorized")
	tr := NewWebSocketRelayTransport(srv.url, "discord", "appShared", RelayTransportOptions{GatewayID: "gw-x", UpgradeSecret: "secret-x",
		Reconnect: true, ReconnectBackoffS: relayFloatPtr(0.05)})
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if !relayWaitUntil(tr.AuthRevoked) {
		t.Fatal("revocation never latched")
	}
	if _, sup, _, _ := tr.testState(); sup {
		t.Fatal("supervisor armed after revocation")
	}
	time.Sleep(200 * time.Millisecond)
	if _, sup, _, _ := tr.testState(); sup {
		t.Fatal("supervisor armed after revocation")
	}
}

func Test4401AfterHandshakeRedialsOnceWithFreshTokenAndRecovers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayExpiredTokenServer(t, func(d int) bool { return d == 1 }, "unauthorized")
	tr := relayAuthTransport(srv.url)
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil || srv.dialCount() != 1 {
		t.Fatalf("err %v dials %d", err, srv.dialCount())
	}
	if !relayWaitUntil(func() bool { d, _, _, _ := tr.testState(); return srv.dialCount() == 2 && d }) {
		t.Fatal("no fresh token redial")
	}
	_, sup, live, connected := tr.testState()
	if tr.AuthRevoked() || !live || !connected || sup {
		t.Fatalf("revoked %v live %v connected %v supervisor %v", tr.AuthRevoked(), live, connected, sup)
	}
	time.Sleep(200 * time.Millisecond)
	if srv.dialCount() != 2 || tr.AuthRevoked() {
		t.Fatalf("dials %d revoked %v", srv.dialCount(), tr.AuthRevoked())
	}
}

func Test4401OnFreshTokenRedialLatchesRevocationAfterExactlyOneRetry(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayExpiredTokenServer(t, func(int) bool { return true }, "unauthorized")
	tr := relayAuthTransport(srv.url)
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if !relayWaitUntil(tr.AuthRevoked) || srv.dialCount() != 2 {
		t.Fatalf("revoked %v dials %d", tr.AuthRevoked(), srv.dialCount())
	}
	time.Sleep(200 * time.Millisecond)
	if _, sup, _, _ := tr.testState(); sup || srv.dialCount() != 2 {
		t.Fatalf("supervisor %v dials %d", sup, srv.dialCount())
	}
}

func Test4401WithReasonExpiredNeverLatchesAndReconnectsNormally(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayExpiredTokenServer(t, func(d int) bool { return d == 1 }, "expired")
	tr := relayAuthTransport(srv.url)
	tr.reconnectBackoffS = 0.05
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if !relayWaitUntil(func() bool { _, sup, _, _ := tr.testState(); return sup }) || tr.AuthRevoked() {
		t.Fatalf("supervisor never armed or revoked %v", tr.AuthRevoked())
	}
	if !relayWaitUntil(func() bool { d, _, _, _ := tr.testState(); return srv.dialCount() == 2 && d }) || tr.AuthRevoked() {
		t.Fatalf("dials %d revoked %v", srv.dialCount(), tr.AuthRevoked())
	}
	if _, _, live, _ := tr.testState(); !live {
		t.Fatal("reader not live after reconnect")
	}
}

func Test4401WithReasonExpiredRepeatedStillNeverLatches(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := relayExpiredTokenServer(t, func(d int) bool { return d == 1 || d == 2 }, "expired")
	tr := relayAuthTransport(srv.url)
	tr.reconnectBackoffS = 0.05
	ctx := context.Background()
	if _, err := tr.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer tr.Disconnect(ctx, nil)
	if _, err := tr.Handshake(ctx); err != nil {
		t.Fatal(err)
	}
	if !relayWaitUntil(func() bool { d, _, _, _ := tr.testState(); return srv.dialCount() == 3 && d }) || tr.AuthRevoked() {
		t.Fatalf("dials %d revoked %v", srv.dialCount(), tr.AuthRevoked())
	}
}
