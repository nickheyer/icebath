package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// What one scripted dial of the loopback connector does
type relayDialScript int

const (
	relayDialRefuse relayDialScript = iota
	relayDialDropAfterHello
	relayDialClose4401AfterHello
	relayDialIdle
)

// Loopback connector playing one script per dial and recording each dial's client address
type relayScriptedServer struct {
	url     string
	mu      sync.Mutex
	scripts []relayDialScript
	addrs   []string
}

func newRelayScriptedServer(t *testing.T, scripts ...relayDialScript) *relayScriptedServer {
	t.Helper()
	s := &relayScriptedServer{scripts: scripts}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		dial := len(s.addrs)
		s.addrs = append(s.addrs, r.RemoteAddr)
		script := relayDialIdle
		if dial < len(s.scripts) {
			script = s.scripts[dial]
		}
		s.mu.Unlock()
		if script == relayDialRefuse {
			http.Error(w, "network blip", http.StatusServiceUnavailable)
			return
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if !strings.Contains(string(data), `"hello"`) {
				continue
			}
			switch script {
			case relayDialDropAfterHello:
				ws.UnderlyingConn().Close()
				return
			case relayDialClose4401AfterHello:
				ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4401, "unauthorized"), time.Now().Add(time.Second))
			}
		}
	}))
	t.Cleanup(srv.Close)
	s.url = "ws" + strings.TrimPrefix(srv.URL, "http")
	return s
}

func (s *relayScriptedServer) dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.addrs)
}

// Whether the transport's live socket is the one the given dial opened
func (s *relayScriptedServer) attachedTo(tr *WebSocketRelayTransport, dial int) bool {
	tr.mu.Lock()
	conn := tr.conn
	tr.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return conn != nil && dial < len(s.addrs) && conn.ws.LocalAddr().String() == s.addrs[dial]
}

// Reconnecting discord transport with zero backoff whose handshake already succeeded once
func relayZeroBackoffTransport(url string) *WebSocketRelayTransport {
	tr := NewWebSocketRelayTransport(url, "discord", "bot", RelayTransportOptions{Reconnect: true,
		ReconnectBackoffS: relayFloatPtr(0), ReconnectMaxBackoffS: relayFloatPtr(0)})
	tr.handshakeSucceeded = true
	return tr
}

func relayReaderLive(tr *WebSocketRelayTransport) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.reader != nil && !tr.reader.finished()
}

func relayDisconnectNow(tr *WebSocketRelayTransport) {
	zero := 0.0
	tr.Disconnect(context.Background(), &zero)
}

func TestProvisional4401NeverStartsASecondDialerWhileTheSupervisorIsMidDial(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayScriptedServer(t, relayDialClose4401AfterHello, relayDialIdle, relayDialIdle)
	tr := relayZeroBackoffTransport(srv.url)
	tr.mu.Lock()
	tr.supervisor = startRelayTask(context.Background(), tr.reconnectLoop)
	tr.mu.Unlock()
	defer relayDisconnectNow(tr)
	if !relayWaitUntil(func() bool { return srv.dials() >= 2 }) {
		t.Fatal("no second dial")
	}
	time.Sleep(100 * time.Millisecond)
	if srv.dials() != 2 {
		t.Fatalf("a second dialer ran alongside the supervisor: %d dials", srv.dials())
	}
	if !relayWaitUntil(func() bool { return srv.attachedTo(tr, 1) }) {
		t.Fatal("the transport must be attached to the live socket")
	}
	if !relayReaderLive(tr) || tr.AuthRevoked() {
		t.Fatal("reader must be live and the credential not revoked")
	}
	tr.mu.Lock()
	retryGen, dialGen := tr.authRetryGeneration, tr.dialGeneration
	tr.mu.Unlock()
	if retryGen == nil || *retryGen != dialGen {
		t.Fatalf("the supervisor's re-dial must carry the fresh token: retry %v dial %d", retryGen, dialGen)
	}
}

func TestFreshTokenRetrySocketDroppingDuringHelloStillReconnects(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayScriptedServer(t, relayDialDropAfterHello, relayDialIdle)
	tr := relayZeroBackoffTransport(srv.url)
	tr.mu.Lock()
	tr.authRetryPending = true
	tr.authRetry = startRelayTask(context.Background(), tr.redialWithFreshToken)
	tr.mu.Unlock()
	defer relayDisconnectNow(tr)
	if !relayWaitUntil(func() bool { return srv.dials() == 2 && srv.attachedTo(tr, 1) }) {
		t.Fatalf("transport ended without the second socket after %d dials", srv.dials())
	}
	if !relayReaderLive(tr) || tr.AuthRevoked() {
		t.Fatal("reader must be live and the credential not revoked")
	}
}

func TestNetworkFailureOnTheRetryDialDoesNotUnmarkIt(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayScriptedServer(t, relayDialRefuse, relayDialClose4401AfterHello, relayDialIdle, relayDialIdle)
	tr := relayZeroBackoffTransport(srv.url)
	tr.mu.Lock()
	tr.authRetryPending = true
	tr.authRetry = startRelayTask(context.Background(), tr.redialWithFreshToken)
	tr.mu.Unlock()
	defer relayDisconnectNow(tr)
	if !relayWaitUntil(tr.AuthRevoked) {
		t.Fatal("the 4401 on the first judged fresh token never latched")
	}
	time.Sleep(50 * time.Millisecond)
	tr.mu.Lock()
	running := tr.dialerRunningLocked()
	tr.mu.Unlock()
	if srv.dials() != 2 || running {
		t.Fatalf("the 4401 on the first judged fresh token must be terminal: %d dials, dialer running %v", srv.dials(), running)
	}
}

func TestSendAllowedOnceRedialInstallsFreshSocket(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayWSServer(t, func(*relayWSServer, *websocket.Conn, int) bool { return true })
	srv.silentOutbound = true
	tr := NewWebSocketRelayTransport(srv.url, "discord", "bot1", RelayTransportOptions{Reconnect: true,
		ReconnectBackoffS: relayFloatPtr(0.01), OutboundTimeoutS: relayFloatPtr(5.0)})
	ctx := context.Background()
	conn, err := tr.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	park := make(chan struct{})
	tr.mu.Lock()
	tr.supervisor = startRelayTask(ctx, func(sctx context.Context) {
		tr.mu.Lock()
		tr.conn = conn
		generation := tr.dialGeneration
		tr.reader = startRelayTask(sctx, func(rctx context.Context) { tr.readLoop(rctx, conn, generation) })
		tr.mu.Unlock()
		tr.send(pyjson.ObjectOf("type", "hello", "platform", "discord", "botId", "bot1"))
		<-park
	})
	supervisor := tr.supervisor
	tr.mu.Unlock()
	defer relayDisconnectNow(tr)
	if !relayWaitUntil(func() bool { return srv.receivedType("hello") }) || supervisor.finished() {
		t.Fatal("the hello window is not open")
	}
	done := make(chan map[string]any, 1)
	go func() {
		r, _ := tr.SendOutbound(ctx, map[string]any{"op": "send_message", "text": "hi"}, "")
		done <- r
	}()
	if !relayWaitUntil(func() bool { return srv.receivedType("outbound") }) || supervisor.finished() {
		t.Fatal("send was rejected during the post-dial window despite a live socket and running reader")
	}
	tr.mu.Lock()
	for _, p := range tr.pending {
		p.fut.settle(map[string]any{"success": true}, nil)
	}
	tr.mu.Unlock()
	select {
	case r := <-done:
		if len(r) != 1 || r["success"] != true {
			t.Fatalf("result %v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("send never resolved")
	}
	close(park)
	<-supervisor.done
}

// Whether the connector received a frame of the type
func (s *relayWSServer) receivedType(kind string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.received {
		if f["type"] == kind {
			return true
		}
	}
	return false
}

func TestSendRaisingSocketReturnsErrorDict(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	srv := newRelayWSServer(t, func(*relayWSServer, *websocket.Conn, int) bool { return true })
	tr := NewWebSocketRelayTransport(srv.url, "discord", "bot1", RelayTransportOptions{OutboundTimeoutS: relayFloatPtr(5.0)})
	ctx := context.Background()
	conn, err := tr.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conn.ws.NetConn().(*net.TCPConn).CloseWrite()
	tr.mu.Lock()
	tr.conn = conn
	tr.reader = startRelayTask(ctx, func(rctx context.Context) { tr.readLoop(rctx, conn, 0) })
	reader := tr.reader
	tr.mu.Unlock()
	r, err := tr.SendOutbound(ctx, map[string]any{"op": "send_message", "text": "hi"}, "")
	if err != nil || r["success"] != false || !strings.Contains(pyrepr.Str(r["error"]), "relay send failed") || tr.pendingCount() != 0 {
		t.Fatalf("result %v err %v", r, err)
	}
	reader.cancel()
	<-reader.done
	relayDisconnectNow(tr)
}
