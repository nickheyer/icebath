package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

const (
	relayWSOpenTimeout   = 10 * time.Second
	relayWSPingInterval  = 30.0
	relayWSPingTimeout   = 60.0
	relayWSMaxSize       = 1 << 20
	relayWSHappyEyeballs = 250 * time.Millisecond
)

// RelayAdmission settles with the off reader admission outcome, context.Canceled for a cancelled one
type RelayAdmission <-chan error

// RelayInboundHandler admits a connector delivered event, a non nil admission deferring the frame ACK
type RelayInboundHandler func(ctx context.Context, event *MessageEvent) (RelayAdmission, error)

// RelayInterruptHandler receives a connector interrupt for a session and chat
type RelayInterruptHandler func(ctx context.Context, sessionKey, chatID string) error

// RelayPassthroughHandler receives a forwarded passthrough request and its buffer id
type RelayPassthroughHandler func(ctx context.Context, forward *PassthroughForward, bufferID any) error

// RelayTransportConn is the gateway to connector transport contract the relay adapter drives
type RelayTransportConn interface {
	Connect(ctx context.Context) (bool, error)
	Disconnect(ctx context.Context, budgetS *float64) error
	Handshake(ctx context.Context) (CapabilityDescriptor, error)
	SetInboundHandler(handler RelayInboundHandler)
	SendOutbound(ctx context.Context, action map[string]any, platform string) (map[string]any, error)
	GetChatInfo(ctx context.Context, chatID string) (map[string]any, error)
	SendInterrupt(ctx context.Context, sessionKey string, reason *string) error
	SendFollowUp(ctx context.Context, action map[string]any, platform string) (map[string]any, error)
}

// One shot settable value a waiter can block on
type relayFuture[T any] struct {
	once sync.Once
	done chan struct{}
	val  T
	err  error
}

func newRelayFuture[T any]() *relayFuture[T] { return &relayFuture[T]{done: make(chan struct{})} }

// Settles the future once, later calls ignored
func (f *relayFuture[T]) settle(val T, err error) {
	f.once.Do(func() {
		f.val, f.err = val, err
		close(f.done)
	})
}

func (f *relayFuture[T]) isDone() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

// Value of the future, a TimeoutError after the timeout and the context error on cancellation
func (f *relayFuture[T]) wait(ctx context.Context, timeoutS float64) (T, error) {
	var zero T
	timer := time.NewTimer(secondsDuration(timeoutS))
	defer timer.Stop()
	select {
	case <-f.done:
		return f.val, f.err
	case <-timer.C:
		return zero, &PyError{"TimeoutError", ""}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Outbound request awaiting its result, remembering its op for reader exit settlement
type relayPending struct {
	op  string
	fut *relayFuture[map[string]any]
}

// Background goroutine with a cancel and a completion signal
type relayTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startRelayTask(ctx context.Context, fn func(ctx context.Context)) *relayTask {
	tctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &relayTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		fn(tctx)
	}()
	return t
}

func (t *relayTask) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Cancels the task and waits for it at most the teardown bound
func (t *relayTask) cancelBounded() {
	t.cancel()
	timer := time.NewTimer(secondsDuration(relayTeardownAwaitTimeoutS))
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	}
}

// Settable event a waiter blocks on until set
type relayEvent struct {
	mu sync.Mutex
	ch chan struct{}
}

func newRelayEvent() *relayEvent { return &relayEvent{ch: make(chan struct{})} }

func (e *relayEvent) set() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.ch:
	default:
		close(e.ch)
	}
}

func (e *relayEvent) clear() {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.ch:
		e.ch = make(chan struct{})
	default:
	}
}

func (e *relayEvent) waitChan() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ch
}

// Close frame code and reason of one side of a closing handshake
type relayCloseFrame struct {
	Code   int
	Reason string
}

// RelayConnectionClosed is a websocket closed abnormally, carrying the received and sent close frames
type RelayConnectionClosed struct {
	Rcvd *relayCloseFrame
	Sent *relayCloseFrame
	Err  error
}

func (e *RelayConnectionClosed) Error() string {
	switch {
	case e.Rcvd != nil:
		return fmt.Sprintf("received %d (%s) %s", e.Rcvd.Code, websocketCloseName(e.Rcvd.Code), e.Rcvd.Reason)
	case e.Sent != nil:
		return fmt.Sprintf("sent %d (%s) %s; no close frame received", e.Sent.Code, websocketCloseName(e.Sent.Code), e.Sent.Reason)
	}
	return "no close frame received or sent"
}

func (e *RelayConnectionClosed) Unwrap() error { return e.Err }

// Explanation websockets gives a close code
func websocketCloseName(code int) string {
	switch code {
	case 1000:
		return "OK"
	case 1001:
		return "going away"
	case 1002:
		return "protocol error"
	case 1003:
		return "unsupported data"
	case 1005:
		return "no status received [internal]"
	case 1006:
		return "connection closed abnormally [internal]"
	case 1007:
		return "invalid frame payload data"
	case 1008:
		return "policy violation"
	case 1009:
		return "message too big"
	case 1010:
		return "mandatory extension"
	case 1011:
		return "internal error"
	case 1012:
		return "service restart"
	case 1013:
		return "try again later"
	case 1014:
		return "bad gateway"
	case 1015:
		return "TLS handshake failure [internal]"
	}
	if code >= 3000 && code < 4000 {
		return "registered"
	}
	if code >= 4000 && code < 5000 {
		return "private use"
	}
	return "unknown"
}

// One dialed socket with its close record and writer lock
type relayConn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	sent    *relayCloseFrame
	rcvd    *relayCloseFrame
	closed  chan struct{}
	pongs   chan string
	once    sync.Once
}

// Writes one text frame under the writer lock, a closed socket failing with its close record
func (c *relayConn) writeText(text string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.TextMessage, []byte(text)); err != nil {
		return c.closedErr(err)
	}
	return nil
}

// Sends a close frame once and drops the TCP connection, recording the sent frame
func (c *relayConn) fail(code int, reason string) {
	c.writeMu.Lock()
	c.mu.Lock()
	if c.sent == nil {
		c.sent = &relayCloseFrame{Code: code, Reason: reason}
		c.mu.Unlock()
		c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	} else {
		c.mu.Unlock()
	}
	c.writeMu.Unlock()
	c.markClosed()
	c.ws.Close()
}

func (c *relayConn) markClosed() { c.once.Do(func() { close(c.closed) }) }

// Sends the close frame, lets a live reader see the reply within the teardown bound, then drops the socket
func (c *relayConn) close(code int, waitReader bool) {
	c.writeMu.Lock()
	c.mu.Lock()
	first := c.sent == nil
	if first {
		c.sent = &relayCloseFrame{Code: code}
	}
	c.mu.Unlock()
	if first {
		c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(secondsDuration(relayTeardownAwaitTimeoutS)))
	}
	c.writeMu.Unlock()
	if waitReader {
		timer := time.NewTimer(secondsDuration(relayTeardownAwaitTimeoutS))
		select {
		case <-c.closed:
		case <-timer.C:
		}
		timer.Stop()
	}
	c.markClosed()
	c.ws.Close()
}

// Close record of the socket as the error an interaction with it raises, nil for a normal closure
func (c *relayConn) closedErr(cause error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ok := func(f *relayCloseFrame) bool { return f != nil && (f.Code == 1000 || f.Code == 1001 || f.Code == 1005) }
	if ok(c.rcvd) && ok(c.sent) {
		return &relayClosedOK{}
	}
	return &RelayConnectionClosed{Rcvd: c.rcvd, Sent: c.sent, Err: cause}
}

// Normal closure of a socket, ending the read loop without an error
type relayClosedOK struct{}

func (*relayClosedOK) Error() string { return "received 1000 (OK); then sent 1000 (OK)" }

// WebSocketRelayTransport is the relay transport over a websocket the gateway dials to the connector
type WebSocketRelayTransport struct {
	url                  string
	platform             string
	botID                string
	identities           [][2]string
	connectTimeoutS      float64
	outboundTimeoutS     float64
	gatewayID            string
	upgradeSecret        string
	reconnect            bool
	reconnectBackoffS    float64
	reconnectMaxBackoffS float64
	dormantRedialS       float64
	redialHoldMaxS       float64

	mu                  sync.Mutex
	conn                *relayConn
	supervisor          *relayTask
	authRetry           *relayTask
	reader              *relayTask
	inbound             RelayInboundHandler
	interruptHandler    RelayInterruptHandler
	passthroughHandler  RelayPassthroughHandler
	descriptor          *CapabilityDescriptor
	byPlatform          map[string]CapabilityDescriptor
	descriptorReady     *relayFuture[CapabilityDescriptor]
	pending             map[string]*relayPending
	goingIdleAck        *relayFuture[struct{}]
	closing             bool
	handshakeSucceeded  bool
	authRevoked         bool
	dialGeneration      int
	authRetryGeneration *int
	authRetryPending    bool
	dormant             bool
	redialHeld          bool
	redialRelease       *relayEvent
}

// RelayTransportOptions are the keyword arguments of the transport constructor
type RelayTransportOptions struct {
	Identities           [][2]string
	ConnectTimeoutS      *float64
	OutboundTimeoutS     *float64
	GatewayID            string
	UpgradeSecret        string
	Reconnect            bool
	ReconnectBackoffS    *float64
	ReconnectMaxBackoffS *float64
}

// Transport dialing the normalized connector URL for the primary platform and every fronted identity
func NewWebSocketRelayTransport(url, platform, botID string, o RelayTransportOptions) *WebSocketRelayTransport {
	pick := func(v *float64, def float64) float64 {
		if v != nil {
			return *v
		}
		return def
	}
	identities := o.Identities
	if len(identities) == 0 {
		identities = [][2]string{{platform, botID}}
	}
	return &WebSocketRelayTransport{
		url: relayWSDialURL(url), platform: platform, botID: botID, identities: append([][2]string(nil), identities...),
		connectTimeoutS: pick(o.ConnectTimeoutS, relayHandshakeTimeoutS), outboundTimeoutS: pick(o.OutboundTimeoutS, relayOutboundTimeoutS),
		gatewayID: o.GatewayID, upgradeSecret: o.UpgradeSecret, reconnect: o.Reconnect,
		reconnectBackoffS: pick(o.ReconnectBackoffS, 1.0), reconnectMaxBackoffS: pick(o.ReconnectMaxBackoffS, 30.0),
		dormantRedialS: 1.0, redialHoldMaxS: relayRedialHoldMaxS, byPlatform: map[string]CapabilityDescriptor{},
		pending: map[string]*relayPending{}, redialRelease: newRelayEvent(),
	}
}

// Opens the socket, starts the reader and sends the hellos
func (t *WebSocketRelayTransport) Connect(ctx context.Context) (bool, error) {
	if err := t.dialAndStart(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Dials, starts the reader and sends one hello per identity, the Discord hello carrying the command manifest
func (t *WebSocketRelayTransport) dialAndStart(ctx context.Context) error {
	t.mu.Lock()
	t.descriptorReady = newRelayFuture[CapabilityDescriptor]()
	t.dialGeneration++
	t.descriptor = nil
	t.byPlatform = map[string]CapabilityDescriptor{}
	t.dormant = false
	t.mu.Unlock()
	conn, err := t.dial(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.conn = conn
	if t.authRetryPending {
		gen := t.dialGeneration
		t.authRetryGeneration = &gen
		t.authRetryPending = false
	}
	generation := t.dialGeneration
	t.reader = startRelayTask(ctx, func(rctx context.Context) { t.readLoop(rctx, conn, generation) })
	reader := t.reader
	t.mu.Unlock()
	for _, id := range t.identities {
		hello := pyjson.ObjectOf("type", "hello", "platform", id[0], "botId", id[1])
		if id[0] == "discord" {
			manifest := BuildRelayCommandManifest()
			rows := make([]any, len(manifest))
			for i, m := range manifest {
				rows[i] = m
			}
			hello.Set("command_manifest", rows)
		}
		if err := t.send(hello); err != nil {
			return err
		}
	}
	if reader.finished() {
		return &PyError{"ConnectionError", "relay ws closed during hello"}
	}
	return nil
}

// Opening handshake with the upgrade bearer, environment proxies, compression and the open timeout
func (t *WebSocketRelayTransport) dial(ctx context.Context) (*relayConn, error) {
	netDialer := &net.Dialer{FallbackDelay: relayWSHappyEyeballs}
	dialer := websocket.Dialer{
		Proxy: pyurl.UrlopenProxy, HandshakeTimeout: relayWSOpenTimeout, EnableCompression: true,
		NetDialContext: netDialer.DialContext,
	}
	header := http.Header{}
	for k, v := range t.upgradeHeaders() {
		header.Set(k, v)
	}
	dctx, cancel := context.WithTimeout(ctx, relayWSOpenTimeout)
	defer cancel()
	ws, resp, err := dialer.DialContext(dctx, t.url, header)
	if err != nil {
		return nil, HandshakeError(resp, err)
	}
	ws.SetReadLimit(relayWSMaxSize)
	c := &relayConn{ws: ws, closed: make(chan struct{}), pongs: make(chan string, 1)}
	ws.SetPongHandler(func(data string) error {
		select {
		case c.pongs <- data:
		default:
		}
		return nil
	})
	ws.SetCloseHandler(func(code int, text string) error {
		c.mu.Lock()
		c.rcvd = &relayCloseFrame{Code: code, Reason: text}
		echo := c.sent == nil
		if echo {
			c.sent = &relayCloseFrame{Code: code, Reason: text}
		}
		c.mu.Unlock()
		if echo {
			c.writeMu.Lock()
			ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
			c.writeMu.Unlock()
		}
		return nil
	})
	go c.keepalive()
	return c, nil
}

// Pings at the interval less the last latency, failing the socket 1011 when a pong misses the timeout
func (c *relayConn) keepalive() {
	latency := 0.0
	for {
		timer := time.NewTimer(secondsDuration(relayWSPingInterval - latency))
		select {
		case <-c.closed:
			timer.Stop()
			return
		case <-timer.C:
		}
		payload := make([]byte, 4)
		rand.Read(payload)
		sentAt := time.Now()
		c.writeMu.Lock()
		err := c.ws.WriteControl(websocket.PingMessage, payload, time.Now().Add(secondsDuration(relayWSPingTimeout)))
		c.writeMu.Unlock()
		if err != nil {
			return
		}
		deadline := time.NewTimer(secondsDuration(relayWSPingTimeout))
	waitPong:
		for {
			select {
			case <-c.closed:
				deadline.Stop()
				return
			case data := <-c.pongs:
				if data == string(payload) {
					latency = time.Since(sentAt).Seconds()
					deadline.Stop()
					break waitPong
				}
			case <-deadline.C:
				c.fail(1011, "keepalive ping timeout")
				return
			}
		}
	}
}

// Upgrade bearer header when both the gateway id and secret are configured
func (t *WebSocketRelayTransport) upgradeHeaders() map[string]string {
	if t.upgradeSecret == "" || t.gatewayID == "" {
		return map[string]string{}
	}
	return map[string]string{"Authorization": "Bearer " + RelayMakeUpgradeToken(t.gatewayID, t.upgradeSecret, relayDefaultUpgradeTTLSeconds)}
}

// Tears the socket down after draining in flight outbound frames within the remaining budget
func (t *WebSocketRelayTransport) Disconnect(ctx context.Context, budgetS *float64) error {
	t.mu.Lock()
	t.closing = true
	var waiting []*relayFuture[map[string]any]
	for _, p := range t.pending {
		if !p.fut.isDone() {
			waiting = append(waiting, p.fut)
		}
	}
	t.mu.Unlock()
	defer func() {
		closed := &PyError{"RuntimeError", "relay transport closed"}
		t.failPending(func(p *relayPending) { p.fut.settle(nil, closed) })
		t.mu.Lock()
		ack := t.goingIdleAck
		t.mu.Unlock()
		if ack != nil {
			ack.settle(struct{}{}, closed)
		}
	}()
	if len(waiting) > 0 {
		if grace := relayDisconnectDrainGrace(budgetS); grace > 0 {
			timer := time.NewTimer(secondsDuration(grace))
		drain:
			for _, f := range waiting {
				select {
				case <-f.done:
				case <-timer.C:
					break drain
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
			timer.Stop()
		}
	}
	for _, slot := range []**relayTask{&t.supervisor, &t.authRetry, &t.reader} {
		t.mu.Lock()
		task := *slot
		t.mu.Unlock()
		if task != nil {
			task.cancelBounded()
			t.mu.Lock()
			*slot = nil
			t.mu.Unlock()
		}
	}
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		conn.close(1000, false)
		t.mu.Lock()
		t.conn = nil
		t.mu.Unlock()
	}
	return nil
}

// Settles every unresolved pending request through settle and clears the table
func (t *WebSocketRelayTransport) failPending(settle func(p *relayPending)) {
	t.mu.Lock()
	pending := make([]*relayPending, 0, len(t.pending))
	for _, p := range t.pending {
		pending = append(pending, p)
	}
	t.pending = map[string]*relayPending{}
	t.mu.Unlock()
	for _, p := range pending {
		if !p.fut.isDone() {
			settle(p)
		}
	}
}

// Connection lost result, possibly delivered for the ops a reader exit leaves ambiguous
func relaySettleConnectionLost(p *relayPending) {
	result := map[string]any{"success": false, "error": "relay transport connection lost"}
	if p.op == relayAmbiguousReaderExitOps {
		result["ambiguous"] = true
	}
	p.fut.settle(result, nil)
}

// Descriptor the connector advertised, waiting up to the connect timeout for the first one
func (t *WebSocketRelayTransport) Handshake(ctx context.Context) (CapabilityDescriptor, error) {
	t.mu.Lock()
	if t.descriptor != nil {
		d := *t.descriptor
		t.mu.Unlock()
		return d, nil
	}
	ready := t.descriptorReady
	t.mu.Unlock()
	if ready == nil {
		return CapabilityDescriptor{}, &PyError{"RuntimeError", "handshake() called before connect()"}
	}
	return ready.wait(ctx, t.connectTimeoutS)
}

// Negotiated descriptor of one fronted platform, nil when none was received
func (t *WebSocketRelayTransport) DescriptorForPlatform(platform string) any {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d, ok := t.byPlatform[platform]; ok {
		return d
	}
	return nil
}

// Negotiated descriptor of one fronted platform
func (t *WebSocketRelayTransport) descriptorFor(platform string) (CapabilityDescriptor, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.byPlatform[platform]
	return d, ok
}

// True once the connector closed 4401 against a fresh token after a successful handshake
func (t *WebSocketRelayTransport) AuthRevoked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.authRevoked
}

func (t *WebSocketRelayTransport) SetInboundHandler(handler RelayInboundHandler) {
	t.mu.Lock()
	t.inbound = handler
	t.mu.Unlock()
}

func (t *WebSocketRelayTransport) SetInterruptInboundHandler(handler RelayInterruptHandler) {
	t.mu.Lock()
	t.interruptHandler = handler
	t.mu.Unlock()
}

func (t *WebSocketRelayTransport) SetPassthroughHandler(handler RelayPassthroughHandler) {
	t.mu.Lock()
	t.passthroughHandler = handler
	t.mu.Unlock()
}

// Carries an outbound action to the connector and returns its result
func (t *WebSocketRelayTransport) SendOutbound(ctx context.Context, action map[string]any, platform string) (map[string]any, error) {
	return t.requestResponse(ctx, action, platform)
}

// Issues a semantic follow up action against a session bound shared identity capability
func (t *WebSocketRelayTransport) SendFollowUp(ctx context.Context, action map[string]any, platform string) (map[string]any, error) {
	return t.requestResponse(ctx, action, platform)
}

// Bot id hello'd for the platform, nil when this transport does not front it
func (t *WebSocketRelayTransport) BotIDFor(platform string) any {
	if platform == "" {
		return nil
	}
	for _, id := range t.identities {
		if id[0] == platform {
			return id[1]
		}
	}
	return nil
}

// Chat name and type the connector reports, the chat id and dm standing in when absent
func (t *WebSocketRelayTransport) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	result, err := t.requestResponse(ctx, map[string]any{"op": "get_chat_info", "chat_id": chatID}, "")
	if err != nil {
		return nil, err
	}
	info := result
	if ci, ok := result["chat_info"]; ok && pyrepr.Truthy(ci) {
		m, ok := ci.(map[string]any)
		if !ok {
			return nil, &PyError{"AttributeError", "'" + pyrepr.TypeName(ci) + "' object has no attribute 'get'"}
		}
		info = m
	}
	name, ok := info["name"]
	if !ok {
		name = chatID
	}
	typ, ok := info["type"]
	if !ok {
		typ = "dm"
	}
	return map[string]any{"name": name, "type": typ}, nil
}

// Routes a mid turn stop for the session to the connector
func (t *WebSocketRelayTransport) SendInterrupt(ctx context.Context, sessionKey string, reason *string) error {
	var r any
	if reason != nil {
		r = *reason
	}
	return t.send(pyjson.ObjectOf("type", "interrupt", "session_key", sessionKey, "reason", r))
}

// Asks the connector to buffer this instance, true only on its acknowledgement within the timeout
func (t *WebSocketRelayTransport) GoIdle(ctx context.Context, timeoutS float64) bool {
	t.mu.Lock()
	if t.conn == nil {
		t.mu.Unlock()
		return false
	}
	ack := newRelayFuture[struct{}]()
	t.goingIdleAck = ack
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.goingIdleAck = nil
		t.mu.Unlock()
	}()
	if err := t.send(pyjson.ObjectOf("type", "going_idle")); err != nil {
		return false
	}
	_, err := ack.wait(ctx, timeoutS)
	return err == nil
}

// Goes idle then closes the socket without closing the transport so the supervisor redials on wake
func (t *WebSocketRelayTransport) GoDormant(ctx context.Context, timeoutS float64) bool {
	t.mu.Lock()
	connected := t.conn != nil
	t.mu.Unlock()
	if !connected {
		return false
	}
	if !t.GoIdle(ctx, timeoutS) {
		return false
	}
	t.mu.Lock()
	t.dormant = true
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		conn.close(1000, true)
	}
	return true
}

// Acknowledges durable receipt of a replayed buffered inbound
func (t *WebSocketRelayTransport) sendInboundAck(bufferID string) {
	if err := t.send(pyjson.ObjectOf("type", "inbound_ack", "bufferId", bufferID)); err != nil {
		wsLog().Debug(fmt.Sprintf("relay: inbound_ack send failed for %s", bufferID))
	}
}

// Outbound frame round trip, every failure a result dict and an unanswered sent frame ambiguous
func (t *WebSocketRelayTransport) requestResponse(ctx context.Context, action map[string]any, platform string) (map[string]any, error) {
	t.mu.Lock()
	if t.closing {
		t.mu.Unlock()
		return map[string]any{"success": false, "error": "relay transport closed"}, nil
	}
	if t.conn == nil {
		t.mu.Unlock()
		return map[string]any{"success": false, "error": "relay transport not connected"}, nil
	}
	requestID := relayUUIDHex()
	op := ""
	if v := action["op"]; pyrepr.Truthy(v) {
		op = pyrepr.Str(v)
	}
	p := &relayPending{op: op, fut: newRelayFuture[map[string]any]()}
	t.pending[requestID] = p
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, requestID)
		t.mu.Unlock()
	}()
	frame := pyjson.ObjectOf("type", "outbound", "requestId", requestID, "action", action)
	if platform != "" {
		frame.Set("platform", platform)
		if botID, ok := t.BotIDFor(platform).(string); ok && botID != "" {
			frame.Set("botId", botID)
		}
	}
	if err := t.send(frame); err != nil {
		wsLog().Debug("relay outbound send failed", "exc_info", err)
		return map[string]any{"success": false, "error": "relay send failed: " + relayExcStr(err)}, nil
	}
	result, err := p.fut.wait(ctx, t.outboundTimeoutS)
	var shape *relayResultShapeError
	if errors.As(err, &shape) {
		return nil, &PyError{"AttributeError", "'" + shape.TypeName + "' object has no attribute 'get'"}
	}
	if err != nil {
		var pe *PyError
		if errors.As(err, &pe) && pe.Kind == "TimeoutError" {
			return map[string]any{"success": false, "error": "relay outbound timed out", "ambiguous": true}, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		wsLog().Debug("relay outbound send failed", "exc_info", err)
		return map[string]any{"success": false, "error": "relay send failed: " + relayExcStr(err), "ambiguous": true}, nil
	}
	return result, nil
}

// Outbound result that is not a mapping, raised where the caller first reads it
type relayResultShapeError struct{ TypeName string }

func (e *relayResultShapeError) Error() string {
	return "'" + e.TypeName + "' object has no attribute 'get'"
}

// Random 32 hex digit request id
func relayUUIDHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b)
}

// One newline terminated JSON text frame
func (t *WebSocketRelayTransport) send(frame *pyjson.Object) error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return &PyError{"RuntimeError", "relay transport not connected"}
	}
	text, err := pyjson.Dumps(frame)
	if err != nil {
		return err
	}
	return conn.writeText(text + "\n")
}

// Reads newline delimited frames until the socket ends, then arms the auth retry or the reconnect supervisor
func (t *WebSocketRelayTransport) readLoop(ctx context.Context, conn *relayConn, generation int) {
	if conn == nil {
		wsLog().Error("relay ws read loop started with no socket")
		t.failPending(relaySettleConnectionLost)
		return
	}
	stop := context.AfterFunc(ctx, func() { conn.ws.SetReadDeadline(time.Now()) })
	defer stop()
	defer func() {
		t.mu.Lock()
		if t.conn == conn && !t.closing {
			t.conn = nil
		}
		t.mu.Unlock()
		t.failPending(relaySettleConnectionLost)
	}()
	authRetryScheduled := false
	err := t.readFrames(ctx, conn)
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	closing := t.closing
	t.mu.Unlock()
	if err != nil {
		code, hasCode := relayCloseCodeOf(err)
		t.mu.Lock()
		succeeded := t.handshakeSucceeded
		t.mu.Unlock()
		if hasCode && code == relayUnauthorizedCloseCode && succeeded {
			reason, _ := relayCloseReasonOf(err)
			t.mu.Lock()
			retryGen := t.authRetryGeneration
			switch {
			case reason == relayExpiredCloseReason:
				t.mu.Unlock()
				if !closing {
					wsLog().Warn("relay ws closed 4401 with reason 'expired' — upgrade token expired, not revoked; reconnecting normally")
				}
			case retryGen != nil && *retryGen == generation:
				t.authRevoked = true
				t.mu.Unlock()
				if !closing {
					wsLog().Warn("relay ws closed 4401 (unauthorized) again on a re-dial with a freshly minted token — treating as a revoked relay credential (opt-out); not reconnecting")
				}
			case !closing:
				authRetryScheduled = true
				t.authRetryPending = true
				t.mu.Unlock()
				wsLog().Warn("relay ws closed 4401 (unauthorized) after a successful handshake — re-dialing once with a fresh upgrade token before treating it as a revoked credential (the token may simply have expired)")
			default:
				t.mu.Unlock()
			}
		} else if !closing {
			wsLog().Warn(fmt.Sprintf("relay ws read loop ended: %s", relayExcStr(err)))
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.closing || t.dialerRunningLocked():
	case authRetryScheduled:
		t.authRetry = startRelayTask(ctx, t.redialWithFreshToken)
	case t.reconnect && !t.authRevoked:
		t.supervisor = startRelayTask(ctx, t.reconnectLoop)
	}
}

// Frames of the socket until it closes, nil for a normal closure and the raised error otherwise
func (t *WebSocketRelayTransport) readFrames(ctx context.Context, conn *relayConn) error {
	buf := ""
	for {
		kind, data, err := conn.ws.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			conn.markClosed()
			closed := conn.closedErr(err)
			if _, ok := closed.(*relayClosedOK); ok {
				return nil
			}
			return closed
		}
		if !utf8.Valid(data) {
			_, derr := pystr.DecodeUTF8(data)
			if kind == websocket.TextMessage {
				conn.fail(1007, derr.Error())
				return conn.closedErr(derr)
			}
			return derr
		}
		buf += string(data)
		lines := strings.Split(buf, "\n")
		buf = lines[len(lines)-1]
		for _, line := range lines[:len(lines)-1] {
			if pystr.Strip(line) == "" {
				continue
			}
			if err := t.handleFrame(ctx, line); err != nil {
				return err
			}
		}
	}
}

// True while the backoff supervisor or the fresh token retry is live
func (t *WebSocketRelayTransport) dialerRunningLocked() bool {
	for _, task := range []*relayTask{t.supervisor, t.authRetry} {
		if task != nil && !task.finished() {
			return true
		}
	}
	return false
}

// Close code of a raised error, the received frame first then the sent one
func relayCloseCodeOf(err error) (int, bool) {
	var cc *RelayConnectionClosed
	if errors.As(err, &cc) {
		for _, f := range []*relayCloseFrame{cc.Rcvd, cc.Sent} {
			if f != nil {
				return f.Code, true
			}
		}
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code, true
	}
	return 0, false
}

// Close reason of a raised error, the received frame first then the sent one
func relayCloseReasonOf(err error) (string, bool) {
	var cc *RelayConnectionClosed
	if errors.As(err, &cc) {
		for _, f := range []*relayCloseFrame{cc.Rcvd, cc.Sent} {
			if f != nil {
				return f.Reason, true
			}
		}
	}
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Text, true
	}
	return "", false
}

// Immediate one shot redial with a freshly minted token after a provisional post handshake 4401
func (t *WebSocketRelayTransport) redialWithFreshToken(ctx context.Context) {
	t.mu.Lock()
	closing := t.closing
	t.mu.Unlock()
	if closing {
		return
	}
	err := t.dialAndStart(ctx)
	if err == nil {
		wsLog().Info("relay ws re-dialed with a fresh upgrade token after 4401")
		return
	}
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	closing = t.closing
	t.mu.Unlock()
	if closing || t.latchIfFreshTokenRefused(err) {
		return
	}
	wsLog().Warn(fmt.Sprintf("relay ws fresh-token re-dial failed: %s", relayExcStr(err)))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.reconnect && !(t.supervisor != nil && !t.supervisor.finished()) {
		t.supervisor = startRelayTask(ctx, t.reconnectLoop)
	}
}

// Latches revocation when a pending fresh token dial was refused 4401 for any reason but expiry
func (t *WebSocketRelayTransport) latchIfFreshTokenRefused(err error) bool {
	code, hasCode := relayCloseCodeOf(err)
	t.mu.Lock()
	if !t.authRetryPending || !hasCode || code != relayUnauthorizedCloseCode {
		t.mu.Unlock()
		return false
	}
	t.authRetryPending = false
	if reason, _ := relayCloseReasonOf(err); reason == relayExpiredCloseReason {
		t.mu.Unlock()
		return false
	}
	t.authRevoked = true
	t.mu.Unlock()
	wsLog().Warn("relay ws fresh-token re-dial rejected with 4401 — treating as a revoked relay credential (opt-out); not reconnecting")
	return true
}

// Redials with capped exponential backoff until a dial succeeds or the transport closes
func (t *WebSocketRelayTransport) reconnectLoop(ctx context.Context) {
	t.mu.Lock()
	backoff := t.reconnectBackoffS
	if t.dormant {
		backoff = t.dormantRedialS
	}
	t.mu.Unlock()
	for !t.isClosing() {
		if sleepSeconds(ctx, backoff) != nil || t.isClosing() {
			return
		}
		if t.awaitRedialHold(ctx) != nil || t.isClosing() {
			return
		}
		err := t.dialAndStart(ctx)
		if err == nil {
			wsLog().Info("relay ws reconnected")
			return
		}
		if ctx.Err() != nil || t.latchIfFreshTokenRefused(err) {
			return
		}
		wsLog().Warn(fmt.Sprintf("relay ws reconnect failed: %s", relayExcStr(err)))
		backoff = min(backoff*2, t.reconnectMaxBackoffS)
	}
}

func (t *WebSocketRelayTransport) isClosing() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closing
}

// Parks the reconnect supervisor until release or the hold cap
func (t *WebSocketRelayTransport) HoldRedial() {
	t.redialRelease.clear()
	t.mu.Lock()
	t.redialHeld = true
	t.mu.Unlock()
}

// Lets the supervisor redial again
func (t *WebSocketRelayTransport) ReleaseRedial() {
	t.mu.Lock()
	t.redialHeld = false
	t.mu.Unlock()
	t.redialRelease.set()
}

// Blocks a pending redial while a brokered suspend is in flight, bounded by the hold cap
func (t *WebSocketRelayTransport) awaitRedialHold(ctx context.Context) error {
	t.mu.Lock()
	held := t.redialHeld
	t.mu.Unlock()
	if !held {
		return nil
	}
	defer func() {
		t.mu.Lock()
		t.redialHeld = false
		t.mu.Unlock()
		t.redialRelease.clear()
	}()
	timer := time.NewTimer(secondsDuration(t.redialHoldMaxS))
	defer timer.Stop()
	select {
	case <-t.redialRelease.waitChan():
	case <-timer.C:
		wsLog().Info("relay: brokered suspend did not land, reconnecting")
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Dispatches one frame by type, a malformed JSON line skipped
func (t *WebSocketRelayTransport) handleFrame(ctx context.Context, line string) error {
	parsed, err := pyjson.Loads(line)
	if err != nil {
		wsLog().Warn("relay: skipping malformed frame")
		return nil
	}
	frame, ok := pyjson.AsObject(parsed)
	if !ok {
		return &PyError{"AttributeError", "'" + pyrepr.TypeName(parsed) + "' object has no attribute 'get'"}
	}
	typ := frame.GetOr("type", nil)
	switch typ.(type) {
	case []any, *pyjson.Object:
		return &PyError{"TypeError", "unhashable type: '" + pyrepr.TypeName(typ) + "'"}
	}
	switch typ {
	case "descriptor":
		return t.onDescriptor(frame)
	case "inbound":
		return t.onInbound(ctx, frame)
	case "going_idle_ack":
		t.mu.Lock()
		ack := t.goingIdleAck
		t.mu.Unlock()
		if ack != nil {
			ack.settle(struct{}{}, nil)
		}
	case "outbound_result":
		rawID := frame.GetOr("requestId", "")
		switch rawID.(type) {
		case []any, *pyjson.Object:
			return &PyError{"TypeError", "unhashable type: '" + pyrepr.TypeName(rawID) + "'"}
		}
		id, _ := rawID.(string)
		t.mu.Lock()
		p := t.pending[id]
		t.mu.Unlock()
		if p != nil {
			rawResult := frame.GetOr("result", pyjson.NewObject())
			if result, ok := pyjson.Plain(rawResult).(map[string]any); ok {
				p.fut.settle(result, nil)
			} else {
				p.fut.settle(nil, &relayResultShapeError{TypeName: pyrepr.TypeName(rawResult)})
			}
		}
	case "interrupt_inbound":
		t.mu.Lock()
		h := t.interruptHandler
		t.mu.Unlock()
		if h != nil {
			return h(ctx, relayWireStr(frame.GetOr("session_key", "")), relayWireStr(frame.GetOr("chat_id", "")))
		}
	case "passthrough_forward":
		t.mu.Lock()
		h := t.passthroughHandler
		t.mu.Unlock()
		if h != nil {
			raw, err := relayWireMapping(frame.GetOr("forward", map[string]any{}))
			if err != nil {
				return err
			}
			fwd := relayPassthroughFromWire(raw)
			return h(RelayCallback(ctx, t, &fwd), &fwd, frame.GetOr("bufferId", nil))
		}
	}
	return nil
}

// Records a negotiated descriptor per platform and resolves the first handshake
func (t *WebSocketRelayTransport) onDescriptor(frame *pyjson.Object) error {
	text, err := pyjson.Dumps(frame.GetOr("descriptor", pyjson.NewObject()))
	if err != nil {
		return err
	}
	d, err := CapabilityDescriptorFromJSON(text)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if d.Platform != "" {
		t.byPlatform[d.Platform] = d
	}
	if t.descriptor == nil {
		dc := d
		t.descriptor = &dc
	}
	t.handshakeSucceeded = true
	ready := t.descriptorReady
	t.mu.Unlock()
	if ready != nil {
		ready.settle(d, nil)
	}
	return nil
}

// Hands an inbound event to the handler, acknowledging its buffer id once admission settles
func (t *WebSocketRelayTransport) onInbound(ctx context.Context, frame *pyjson.Object) error {
	t.mu.Lock()
	handler := t.inbound
	t.mu.Unlock()
	if handler == nil {
		return nil
	}
	raw, err := relayWireMapping(frame.GetOr("event", map[string]any{}))
	if err != nil {
		return err
	}
	event, err := relayEventFromWire(ctx, raw)
	if err != nil {
		return err
	}
	admitting, herr := relayCallInbound(RelayCallback(ctx, t, event), handler, event)
	if herr != nil {
		wsLog().Error("relay: inbound handler failed; frame left unacknowledged", "exc_info", herr)
		return nil
	}
	bufferID := frame.GetOr("bufferId", nil)
	if admitting != nil {
		go t.ackAdmitted(admitting, bufferID)
	} else if pyrepr.Truthy(bufferID) {
		t.sendInboundAck(pyrepr.Str(bufferID))
	}
	return nil
}

// Calls the inbound handler with a panic surfacing as its error
func relayCallInbound(ctx context.Context, handler RelayInboundHandler, event *MessageEvent) (admission RelayAdmission, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", rec)
		}
	}()
	return handler(ctx, event)
}

// Acknowledges a frame once its off reader admission committed, a cancelled or failed one left for redelivery
func (t *WebSocketRelayTransport) ackAdmitted(admission RelayAdmission, bufferID any) {
	err := <-admission
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			wsLog().Error("relay: inbound handler failed; frame left unacknowledged", "exc_info", err)
		}
		return
	}
	if pyrepr.Truthy(bufferID) {
		t.sendInboundAck(pyrepr.Str(bufferID))
	}
}

// Connector URL the transport dials
func (t *WebSocketRelayTransport) RelayURL() string { return t.url }

// Enrolled gateway id presented on the upgrade
func (t *WebSocketRelayTransport) RelayGatewayID() string { return t.gatewayID }

// Per gateway upgrade secret
func (t *WebSocketRelayTransport) RelayUpgradeSecret() string { return t.upgradeSecret }

// True once revocation latched
func (t *WebSocketRelayTransport) RelayAuthRevoked() bool { return t.AuthRevoked() }

// True once disconnect began
func (t *WebSocketRelayTransport) RelayClosing() bool { return t.isClosing() }

// True while a socket is held
func (t *WebSocketRelayTransport) RelayConnected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn != nil
}

// Fronted platform and bot id pairs as JSON ready lists
func (t *WebSocketRelayTransport) RelayIdentities() any {
	out := make([]any, len(t.identities))
	for i, id := range t.identities {
		out[i] = []any{id[0], id[1]}
	}
	return out
}
