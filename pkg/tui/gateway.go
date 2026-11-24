package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

const (
	maxGatewayLogLines  = 200
	maxLogLineBytes     = 4096
	maxBufferedEvents   = 2000
	maxLogPreview       = 240
	heartbeatInterval   = 15 * time.Second
	heartbeatDeadline   = 45 * time.Second
	lateTickIntervals   = 1.5
	maxOutstandingPings = 8
	reconnectBase       = time.Second
	reconnectMax        = 30 * time.Second
	ownerRecoveryLimit  = 3
	ownerRecoveryWindow = time.Minute
	bootstrapTimeout    = 40 * time.Second
	closeAbnormal       = websocket.CloseAbnormalClosure
)

// Error answer of a gateway request, its code and message as the server sent them
type rpcError struct {
	Code    int64
	HasCode bool
	Message string
	Data    map[string]any
}

func (e *rpcError) Error() string { return e.Message }

// Event the gateway emitted, its type, session and payload, with the frame params it rode on
type gatewayEventMsg struct {
	Type      string
	SessionID string
	Payload   map[string]any
	Frame     tuigateway.EventEnvelope
	// Owner epoch and execution generation a canonical frame or its payload stamps, nil when unstamped
	Epoch      *int64
	Generation *int64
	// Typed body of an event the client raises about its own link, nil for a wire event
	Local any
}

// Line the gateway link reported on its error stream
type gatewayStderr struct{ Line string }

// Redial of a dropped link, its attempt number and the delay before it
type gatewayReconnecting struct {
	Attempt int
	DelayMs float64
}

// Gateway that did not come up in time, the tail of its error output
type gatewayStartTimeout struct{ StderrTail string }

// Frame that was no message the client could read, its first characters
type gatewayProtocolError struct{ Preview string }

// Request the gateway sent the terminal UI, answered once through respond, fail or decline
type serverRequest struct {
	ID       any
	Method   string
	Params   map[string]any
	Replayed bool

	client  *gatewayClient
	mu      sync.Mutex
	settled bool
}

// Server request delivered to the program
type serverRequestMsg struct{ Request *serverRequest }

// Gateway transport ended with its close code, the client already scheduling its reconnect
type gatewayExitMsg struct{ Code int }

// Clock the client schedules its timers on, wall time in production
type gatewayClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// Request timeout of the settings
func requestTimeout() time.Duration { return prefs.RequestTimeout }

// Wait for gateway.ready of the settings
func startupTimeout() time.Duration { return prefs.StartupTimeout }

// Explicit gateway WebSocket the TUI attaches to instead of the local owner, empty when unset
func attachURLEnv() string { return strings.TrimFunc(os.Getenv("ICEBATH_TUI_GATEWAY_URL"), isJSSpace) }

// Whether a bootstrap failure is the verdict of a helper that cannot load icebath, which no retry fixes
func isTerminalBootstrapFailure(message string) bool {
	return terminalBootstrapLine(message) != ""
}

// First line of a terminal bootstrap verdict, empty when the failure may pass
func terminalBootstrapLine(message string) string {
	for line := range strings.SplitSeq(message, "\n") {
		if strings.HasPrefix(line, "gateway unavailable: ") {
			return line
		}
	}
	return ""
}

// Owner attach of the launcher held to the bootstrap deadline
func boundedAttach(attach func(context.Context, bool, bool) (Grant, error)) func(context.Context, bool, bool) (Grant, error) {
	return func(ctx context.Context, start, recover bool) (Grant, error) {
		ctx, cancel := context.WithTimeout(ctx, bootstrapTimeout)
		defer cancel()
		return attach(ctx, start, recover)
	}
}

// Lookup over the environment the TUI launched with, kept before the bootstrap scrubs the process env
func launchEnv() func(string) string {
	snapshot := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			snapshot[k] = v
		}
	}
	return func(name string) string { return snapshot[name] }
}

// One socket generation, settled once its dial finished
type gatewayConn struct {
	url     string
	settled chan struct{}
	dialErr error
	cancel  context.CancelFunc

	sock   wireSocket
	link   *wireLink
	open   bool
	closed bool
}

// Closes the socket of a generation, its reader then reporting the close
func (g *gatewayConn) shut() {
	g.cancel()
	if g.link != nil {
		go g.link.close()
	}
}

// Request in flight on a socket generation, failed with the cause the transport detached it with
type pendingCall struct{ cancel context.CancelCauseFunc }

// Hold of one session's canonical frames while its resume is in flight
type attachHold struct {
	sid    string
	frames []gatewayEventMsg
}

// Shared gateway client with bootstrap, attach, readiness, heartbeat and reconnect
type gatewayClient struct {
	bootstrap func(ctx context.Context, start, recover bool) (Grant, error)
	dial      func(ctx context.Context, target string, protocols []string) (wireSocket, error)
	clock     gatewayClock
	env       func(string) string
	timeout   time.Duration

	mu        sync.Mutex
	conn      *gatewayConn
	attachURL string
	logs      []string
	pending   map[*pendingCall]struct{}

	sink           func(tea.Msg)
	backlog        []tea.Msg
	bufferedReqs   []tea.Msg
	pendingExit    *int
	outbox         []tea.Msg
	delivering     bool
	ready          bool
	readyStop      func() bool
	readyToken     *int
	reconnectStop  func() bool
	reconnectToken *int
	reconnects     int
	disposed       bool

	bootstrapFlight chan struct{}
	bootstrapErr    error
	localStarted    bool
	localGeneration int
	recoveries      []time.Time

	// Sessions whose frames are held while their resume is in flight, each session card resuming its own
	holds map[string]*attachHold
	// Request ids of creates that failed with no clear answer, by what was asked for, so the next create asking for the same adopts the session
	// the owner may have made instead of making a second one
	unsettled map[string][]any

	heartbeatToken *int
	heartbeatStop  func() bool
	lastLiveness   time.Time
	lastPingAt     time.Time
	lastTickAt     time.Time
	previousLate   bool
	graceSent      bool
	pings          []string
	pingSeq        int
}

// Client of the local shared gateway reached through the launcher's attach, started by start and subscribed by attach
func newGatewayClient(attach func(context.Context, bool, bool) (Grant, error)) *gatewayClient {
	return &gatewayClient{bootstrap: boundedAttach(attach), dial: dialGatewaySocket, clock: systemClock{}, env: launchEnv(), timeout: requestTimeout()}
}

// Whether the client attaches an explicit or granted WebSocket
func (c *gatewayClient) attached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attachURL != ""
}

var _ turnGateway = (*gatewayClient)(nil)

// Hands every message to send in order, the buffered ones first, never inside this call
func (c *gatewayClient) attach(send func(tea.Msg)) {
	c.mu.Lock()
	c.sink = send
	c.flushBufferedLocked()
	c.mu.Unlock()
	go c.deliver()
}

// Moves the buffered events, then requests, then the pending exit into the outbox of a subscribed client
func (c *gatewayClient) flushBufferedLocked() {
	if c.sink == nil {
		return
	}
	c.outbox = append(c.outbox, c.backlog...)
	c.outbox = append(c.outbox, c.bufferedReqs...)
	c.backlog, c.bufferedReqs = nil, nil
	if c.pendingExit != nil {
		c.outbox = append(c.outbox, gatewayExitMsg{Code: *c.pendingExit})
		c.pendingExit = nil
	}
}

// Delivers the outbox in order outside the lock, one deliverer at a time
func (c *gatewayClient) deliver() {
	c.mu.Lock()
	if c.delivering || c.sink == nil {
		c.mu.Unlock()
		return
	}
	c.delivering = true
	for len(c.outbox) > 0 {
		m, sink := c.outbox[0], c.sink
		c.outbox = c.outbox[1:]
		c.mu.Unlock()
		sink(m)
		c.mu.Lock()
	}
	c.delivering = false
	c.mu.Unlock()
}

// Queues an event for the subscriber, buffering the newest two thousand before it subscribed
func (c *gatewayClient) publishLocked(ev gatewayEventMsg) {
	if ev.Type == "gateway.ready" {
		c.ready = true
		c.clearReconnectLocked()
		c.reconnects = 0
		c.clearReadyTimerLocked()
		if c.conn != nil && c.conn.open {
			c.startHeartbeatLocked()
		}
	}
	if c.sink != nil {
		c.outbox = append(c.outbox, ev)
		return
	}
	c.backlog = append(c.backlog, ev)
	if len(c.backlog) > maxBufferedEvents {
		c.backlog = c.backlog[len(c.backlog)-maxBufferedEvents:]
	}
}

// Runs a locked step, then delivers what it queued
func (c *gatewayClient) locked(step func()) {
	c.mu.Lock()
	step()
	c.mu.Unlock()
	c.deliver()
}

// Runs a locked step for a caller on the update loop and delivers what it queued from another goroutine: the program takes no message while it
// updates, so a delivery made right there would wait on itself for good
func (c *gatewayClient) lockedOnLoop(step func()) {
	c.mu.Lock()
	step()
	c.mu.Unlock()
	go c.deliver()
}

// Event of a gateway frame's params, its payload an object or empty
func wireEvent(params map[string]any) gatewayEventMsg {
	ev := gatewayEventMsg{Payload: map[string]any{}}
	_ = tuigateway.Loose(params, &ev.Frame)
	ev.Type, ev.SessionID = ev.Frame.Type, ev.Frame.SessionID
	if payload, isDict := params["payload"].(map[string]any); isDict {
		ev.Payload = payload
	}
	return ev
}

// Stamp of a canonical frame, the frame's own first and the payload's when the frame carries none
func frameStamp(frame *int64, payload map[string]any, key string) *int64 {
	if frame != nil {
		return frame
	}
	var stamp struct {
		Value *int64 `json:"value"`
	}
	if raw, has := payload[key]; has && tuigateway.Loose(map[string]any{"value": raw}, &stamp) == nil {
		return stamp.Value
	}
	return nil
}

// Event off the socket, stamped with the service's execution epoch and held for a pending attach
func (c *gatewayClient) publishWireLocked(conn *gatewayConn, ev gatewayEventMsg) {
	if ev.Type == "gateway.ready" {
		c.publishLocked(ev)
		return
	}
	ev.Epoch = frameStamp(ev.Frame.AuthorityEpoch, ev.Payload, "authority_epoch")
	ev.Generation = frameStamp(ev.Frame.ExecutionGeneration, ev.Payload, "execution_generation")
	if hold := c.holds[ev.Frame.SessionID]; hold != nil {
		hold.frames = append(hold.frames, ev)
		return
	}
	c.publishLocked(ev)
}

// Text of a failure as JavaScript String renders an Error
func jsErrorText(err error) string {
	var refusal *rpcError
	if errors.As(err, &refusal) {
		return "JsonRpcGatewayError: " + refusal.Message
	}
	return "Error: " + err.Error()
}

func (c *gatewayClient) clearReadyTimerLocked() {
	if c.readyStop != nil {
		c.readyStop()
		c.readyStop, c.readyToken = nil, nil
	}
}

// Closes the current socket generation, its late close then logged as stale
func (c *gatewayClient) closeGatewaySocketLocked() {
	conn := c.conn
	c.conn = nil
	if conn != nil {
		conn.shut()
	}
}

// Drops a socket whose heartbeat went unanswered for a full deadline so the close path reconnects
func (c *gatewayClient) onHeartbeatFailureLocked() {
	conn := c.conn
	if conn == nil {
		return
	}
	c.lifecycleLocked("[lifecycle] websocket silent drop detected (heartbeat ack timeout); forcing reconnect")
	conn.shut()
}

// Reconnect delay of an attempt, one second doubling to thirty without jitter
func reconnectDelay(attempt int) time.Duration {
	exponent := min(max(0, attempt), 32)
	return time.Duration(math.Min(float64(reconnectMax), float64(reconnectBase)*math.Pow(2, float64(exponent))))
}

func (c *gatewayClient) scheduleReconnectLocked() {
	if c.disposed || c.reconnectStop != nil {
		return
	}
	delay := reconnectDelay(c.reconnects)
	c.reconnects++
	ms := delay.Milliseconds()
	c.lifecycleLocked(fmt.Sprintf("[lifecycle] scheduling gateway reconnect in %dms (attempt %d)", ms, c.reconnects))
	c.publishLocked(gatewayEventMsg{Type: "gateway.reconnecting", Local: gatewayReconnecting{Attempt: c.reconnects, DelayMs: float64(ms)}})
	token := new(int)
	c.reconnectToken = token
	c.reconnectStop = c.clock.AfterFunc(delay, func() {
		c.locked(func() {
			if c.reconnectToken != token {
				return
			}
			c.reconnectStop, c.reconnectToken = nil, nil
			if c.disposed {
				return
			}
			c.startLocked()
			c.flushBufferedLocked()
		})
	})
}

func (c *gatewayClient) clearReconnectLocked() {
	if c.reconnectStop != nil {
		c.reconnectStop()
		c.reconnectStop, c.reconnectToken = nil, nil
	}
}

// Fails every request in flight with the cause and unbinds the heartbeat
func (c *gatewayClient) detachLocked(cause error) {
	c.stopHeartbeatLocked()
	for call := range c.pending {
		call.cancel(cause)
	}
	c.pending = nil
}

// Rejects the old transport's requests and buffered frames before a new transport starts
func (c *gatewayClient) resetStartupStateLocked() {
	c.detachLocked(errors.New(tr(txt.LibTextGatewayRestarting)))
	c.ready = false
	c.backlog, c.bufferedReqs, c.pendingExit = nil, nil, nil
	c.clearReadyTimerLocked()
}

// Publishes gateway.start_timeout with the log tail when readiness has not arrived in time
func (c *gatewayClient) startReadyTimerLocked(python, cwd string) {
	token := new(int)
	c.readyToken = token
	c.readyStop = c.clock.AfterFunc(startupTimeout(), func() {
		c.locked(func() {
			if c.readyToken != token || c.ready {
				return
			}
			tail := c.logTailLocked(20)
			c.lifecycleLocked(fmt.Sprintf("[startup] timed out waiting for gateway.ready (python=%s, cwd=%s)", python, cwd))
			c.publishLocked(gatewayEventMsg{Type: "gateway.start_timeout", Local: gatewayStartTimeout{StderrTail: tail}})
		})
	})
}

// Fails the transport's requests, schedules the reconnect and tells the subscriber the exit code
func (c *gatewayClient) handleTransportExitLocked(code int, reason string) {
	c.clearReadyTimerLocked()
	c.ready = false
	reasonText := reason
	if reasonText == "" {
		reasonText = "none"
	}
	c.lifecycleLocked(fmt.Sprintf("[lifecycle] transport exit code=%d reason=%s", code, reasonText))
	if reason == "" {
		reason = tr(txt.LibTextGatewayExitedWithCode, code)
	}
	c.detachLocked(errors.New(reason))
	c.scheduleReconnectLocked()
	if c.sink != nil {
		c.outbox = append(c.outbox, gatewayExitMsg{Code: code})
		return
	}
	c.pendingExit = &code
}

// Starts holding a session's frames while its resume is in flight. A newer resume of the same session takes over the frames the older
// one held, so none is lost when the older one settles
func (c *gatewayClient) holdAttach(sessionID string) *attachHold {
	c.mu.Lock()
	defer c.mu.Unlock()
	hold := &attachHold{sid: sessionID}
	if older := c.holds[sessionID]; older != nil {
		hold.frames = older.frames
	}
	if c.holds == nil {
		c.holds = map[string]*attachHold{}
	}
	c.holds[sessionID] = hold
	return hold
}

// Ends a hold, replaying only the frames past the applied snapshot's fence, or all of them without one; a hold a newer resume took over
// ends with nothing to replay
func (c *gatewayClient) settleAttach(hold *attachHold, snapshot *tuigateway.CanonicalSnapshot) {
	c.lockedOnLoop(func() {
		if hold == nil || c.holds[hold.sid] != hold {
			return
		}
		delete(c.holds, hold.sid)
		for _, ev := range hold.frames {
			if snapshot == nil {
				c.publishLocked(ev)
				continue
			}
			seq := ev.Frame.Seq
			if ev.Frame.ReplayEpoch != nil && *ev.Frame.ReplayEpoch == snapshot.ReplayEpoch && seq != nil && *seq > snapshot.LastSequence {
				c.publishLocked(ev)
			}
		}
	})
}

// Publishes the shared prompts of a session authority snapshot and delivers the open server requests of an attached session again on either route:
// the copy that came with the attach answer arrived while the view still showed the session it was leaving, and the reset of the view dropped it
func (c *gatewayClient) hydrateSharedPrompts(snapshot *tuigateway.CanonicalSnapshot, open []tuigateway.OpenRequestEntry) {
	c.lockedOnLoop(func() {
		if snapshot != nil {
			for _, prompt := range snapshot.Prompts {
				generation := prompt.ExecutionGeneration
				c.publishLocalEventLocked(gatewayEventMsg{Type: string(prompt.Kind) + ".request", SessionID: snapshot.SessionID,
					Payload: wireParams(prompt), Epoch: ptr(snapshot.AuthorityEpoch), Generation: &generation})
			}
		}
		for _, req := range open {
			params := req.Params
			if params == nil {
				params = map[string]any{}
			}
			c.deliverRequestLocked(req.ID, req.Method, params, true)
		}
	})
}

// Publishes a client side event as an event frame
func (c *gatewayClient) publishLocalEvent(ev gatewayEventMsg) {
	c.lockedOnLoop(func() { c.publishLocalEventLocked(ev) })
}

func (c *gatewayClient) publishLocalEventLocked(ev gatewayEventMsg) {
	ev.Frame = tuigateway.EventEnvelope{Type: ev.Type, SessionID: ev.SessionID, Payload: ev.Payload}
	c.publishLocked(ev)
}

// Logs a frame the client could not read and publishes gateway.protocol_error
func (c *gatewayClient) protocolErrorLocked(what, text, emptyLabel string) {
	preview := sliceUnits(strings.TrimFunc(text, isJSSpace), maxLogPreview)
	if preview == "" {
		preview = emptyLabel
	}
	c.pushLogLocked("[protocol] " + what + ": " + preview)
	c.publishLocked(gatewayEventMsg{Type: "gateway.protocol_error", Local: gatewayProtocolError{Preview: preview}})
}

// Runs the local bootstrap, starting the owner on the first launch and re-ensuring a lost one within the budget
func (c *gatewayClient) startLocalGatewayLocked() {
	c.localGeneration++
	generation := c.localGeneration
	start := !c.localStarted
	recover := !start && c.claimOwnerRecoveryLocked(c.clock.Now())
	c.localStarted = true
	c.bootstrapErr = nil
	flight := make(chan struct{})
	c.bootstrapFlight = flight
	go func() {
		grant, err := c.bootstrap(context.Background(), start, recover)
		c.locked(func() {
			defer close(flight)
			if c.disposed || generation != c.localGeneration {
				return
			}
			if err == nil {
				c.attachURL = grant.URL
				mode := "discover"
				if start {
					mode = "start"
				} else if recover {
					mode = "recover"
				}
				if grant.Replaced != "" {
					c.lifecycleLocked(fmt.Sprintf("[lifecycle] replaced idle shared gateway of build %s by this build %s", grant.Replaced, tuigateway.BuildID()))
				}
				c.lifecycleLocked(fmt.Sprintf("[lifecycle] attached shared gateway %s url=%s profile=%s instance=%s", mode, redactURL(grant.URL), grant.ProfileID, grant.InstanceID))
				c.startAttachedGatewayLocked(grant.URL, grant.Protocols)
				return
			}
			c.bootstrapErr = err
			c.clearReadyTimerLocked()
			c.publishLocked(gatewayEventMsg{Type: "gateway.start_timeout", Local: gatewayStartTimeout{StderrTail: err.Error()}})
			c.detachLocked(err)
			if line := terminalBootstrapLine(err.Error()); line != "" {
				c.lifecycleLocked("[lifecycle] gateway bootstrap cannot start; not retrying: " + line)
				return
			}
			c.scheduleReconnectLocked()
		})
	}()
}

// Takes one slot of the sliding owner re-ensure budget, false once three were used within a minute
func (c *gatewayClient) claimOwnerRecoveryLocked(now time.Time) bool {
	kept := c.recoveries[:0]
	for _, at := range c.recoveries {
		if now.Sub(at) < ownerRecoveryWindow {
			kept = append(kept, at)
		}
	}
	c.recoveries = kept
	if len(c.recoveries) >= ownerRecoveryLimit {
		return false
	}
	c.recoveries = append(c.recoveries, now)
	return true
}

// Whether a URL is one a WebSocket constructor accepts, ws or wss with a host and a valid port
func constructibleWSURL(raw string) bool {
	u, ok := parseWHATWG(raw)
	return ok && (u.scheme == "ws" || u.scheme == "wss") && u.host != ""
}

// Dials a socket generation and wires its reader, describing the canonical runtime once it opens
func (c *gatewayClient) startAttachedGatewayLocked(target string, protocols []string) {
	safe := redactURL(target)
	c.startReadyTimerLocked("websocket", safe)
	if !constructibleWSURL(target) {
		c.pushLogLocked(fmt.Sprintf("[startup] failed to connect websocket gateway %s (constructor error)", safe))
		c.handleTransportExitLocked(1, tr(txt.LibTextGatewayWebsocketStartupFailed))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &gatewayConn{url: target, settled: make(chan struct{}), cancel: cancel}
	c.conn = conn
	c.stopHeartbeatLocked()
	c.lastLiveness = c.clock.Now()
	go func() {
		sock, err := c.dial(ctx, target, protocols)
		c.locked(func() {
			defer close(conn.settled)
			if err != nil {
				c.pushLogLocked("[startup] gateway websocket connect error")
				conn.dialErr = errors.New(tr(txt.LibTextGatewayWebsocketConnectionFailed))
				c.transportErrorLocked(conn)
				c.onSocketCloseLocked(conn, closeAbnormal)
				return
			}
			if c.conn != conn || ctx.Err() != nil {
				conn.dialErr = errors.New(tr(txt.LibTextGatewayWebsocketConnectionFailed))
				go sock.Close()
				c.pushLogLocked(fmt.Sprintf("[lifecycle] stale websocket close ignored code=%d", closeAbnormal))
				return
			}
			link := newWireLink(sock, func(frame map[string]any) { c.inbound(conn, frame) }, func(raw string) { c.malformed(conn, raw) })
			conn.sock, conn.link, conn.open = sock, link.start(), true
			go c.pump(conn)
		})
	}()
}

// Logs a transport error of the current socket and publishes it as a gateway.stderr line
func (c *gatewayClient) transportErrorLocked(conn *gatewayConn) {
	if c.conn != conn {
		return
	}
	line := "[gateway] websocket transport error"
	c.pushLogLocked(line)
	c.publishLocked(gatewayEventMsg{Type: "gateway.stderr", Local: gatewayStderr{Line: line}})
}

// Liveness and heartbeat acknowledgement of every frame the current socket reads
func (c *gatewayClient) inbound(conn *gatewayConn, frame map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != conn {
		return
	}
	c.lastLiveness = c.clock.Now()
	if id, isStr := frame["id"].(string); isStr {
		if i := slices.Index(c.pings, id); i >= 0 {
			c.pings = slices.Delete(c.pings, i, i+1)
		}
	}
}

// Logs and publishes a frame of the current socket that is not a JSON object, the socket staying up
func (c *gatewayClient) malformed(conn *gatewayConn, raw string) {
	c.locked(func() {
		if c.conn != conn || raw == "" {
			return
		}
		c.protocolErrorLocked("malformed websocket frame", raw, "(empty frame)")
	})
}

// Routes the frames of one socket generation to the subscriber until its reader ends with the close
func (c *gatewayClient) pump(conn *gatewayConn) {
	for {
		frame, more := conn.link.next()
		if !more {
			code, clean := closeCode(conn.link.readError())
			c.locked(func() {
				conn.closed = true
				if !clean {
					c.transportErrorLocked(conn)
				}
				c.onSocketCloseLocked(conn, code)
			})
			return
		}
		c.route(conn, frame)
	}
}

// Close code of a finished read, abnormal when no close frame arrived
func closeCode(err error) (int, bool) {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code != closeAbnormal {
		return closeErr.Code, true
	}
	return closeAbnormal, false
}

// Logs a close and, for the current socket, ends the transport onto the reconnect path
func (c *gatewayClient) onSocketCloseLocked(conn *gatewayConn, code int) {
	if c.conn != conn {
		c.pushLogLocked(fmt.Sprintf("[lifecycle] stale websocket close ignored code=%d", code))
		return
	}
	c.pushLogLocked(fmt.Sprintf("[lifecycle] websocket close code=%d", code))
	c.conn = nil
	reason := tr(txt.LibTextGatewayWebsocketClosed)
	if code != 0 {
		reason = tr(txt.LibTextGatewayWebsocketClosedWithCode, code)
	}
	c.handleTransportExitLocked(code, reason)
}

// Routes one frame of the current socket, an event to the wire publisher and a method with an id as a server request
func (c *gatewayClient) route(conn *gatewayConn, frame map[string]any) {
	c.locked(func() {
		if c.conn != conn {
			return
		}
		method, isMethod := frame["method"].(string)
		id, isStrID := frame["id"].(string)
		if isMethod && isStrID && method != "event" {
			params, _ := frame["params"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			c.deliverRequestLocked(id, method, params, false)
			return
		}
		params, _ := frame["params"].(map[string]any)
		if method != "event" || params == nil {
			return
		}
		if _, isType := params["type"].(string); !isType {
			return
		}
		ev := wireEvent(params)
		if ev.Type == "gateway.ready" {
			c.advertiseCapabilitiesLocked(conn)
		}
		c.publishWireLocked(conn, ev)
	})
}

// Tells the service once per ready that this window answers server requests
func (c *gatewayClient) advertiseCapabilitiesLocked(conn *gatewayConn) {
	go func() {
		value, err := c.call(context.Background(), conn, tuigateway.ClientCapabilities.Name(), wireParams(tuigateway.ClientCapabilitiesParams{ServerRequests: true}), 0)
		if err != nil {
			return
		}
		if _, err := tuigateway.ClientCapabilities.Result(value); err != nil {
			c.recordLog("[protocol] " + misfitText(tr(txt.SessionCommonInvalidResponse, tuigateway.ClientCapabilities.Name())) + " " + err.Error())
		}
	}()
}

// Queues a server request for the subscriber, buffering it before the subscriber attached
func (c *gatewayClient) deliverRequestLocked(id any, method string, params map[string]any, replayed bool) {
	m := serverRequestMsg{Request: &serverRequest{ID: id, Method: method, Params: params, Replayed: replayed, client: c}}
	if c.sink != nil {
		c.outbox = append(c.outbox, m)
		return
	}
	c.bufferedReqs = append(c.bufferedReqs, m)
}

// Sends one answer frame for the request on the current socket, the first answer only
func (r *serverRequest) send(member string, value any) {
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		return
	}
	r.settled = true
	r.mu.Unlock()
	id, err := json.Marshal(r.ID)
	if err != nil {
		return
	}
	body, err := json.Marshal(value)
	if err != nil {
		return
	}
	raw := `{"jsonrpc":"2.0","id":` + string(id) + `,"` + member + `":` + string(body) + `}`
	r.client.mu.Lock()
	conn := r.client.conn
	r.client.mu.Unlock()
	if conn != nil && conn.link != nil {
		_ = conn.link.sendFrame(raw)
	}
}

// Answers the request with a result
func (r *serverRequest) respond(result any) { r.send("result", result) }

// Answers the request with an error
func (r *serverRequest) fail(code int, message string) {
	r.send("error", map[string]any{"code": code, "message": message})
}

// Answers a request no handler took with method not found and logs it
func (r *serverRequest) unhandled() {
	r.fail(jsonRPCMethodNotFound, "no handler for server request: "+r.Method)
	r.client.recordLog("[protocol] unhandled server request: " + r.Method)
}

// Starts the transport, an explicit ICEBATH_TUI_GATEWAY_URL attach or the local owner through the bootstrap
func (c *gatewayClient) start() {
	c.locked(c.startLocked)
}

func (c *gatewayClient) startLocked() {
	if c.disposed {
		c.pushLogLocked("[lifecycle] start() ignored after kill()")
		return
	}
	c.clearReconnectLocked()
	c.attachURL = attachURLEnv()
	c.resetStartupStateLocked()
	c.closeGatewaySocketLocked()
	if c.attachURL != "" {
		c.startAttachedGatewayLocked(c.attachURL, nil)
		return
	}
	c.startLocalGatewayLocked()
}

// Line capped at four kilobytes with the truncation marker the logs view shows
func truncateLine(line string) string {
	if n := unitLen(line); n > maxLogLineBytes {
		return fmt.Sprintf("%s… [truncated %d bytes]", sliceUnits(line, maxLogLineBytes), n)
	}
	return line
}

func (c *gatewayClient) pushLogLocked(line string) {
	c.logs = append(c.logs, truncateLine(line))
	if len(c.logs) > maxGatewayLogLines {
		c.logs = c.logs[len(c.logs)-maxGatewayLogLines:]
	}
}

// Keeps a client side diagnostic line in the logs tail
func (c *gatewayClient) recordLog(line string) {
	c.mu.Lock()
	c.pushLogLocked(line)
	c.mu.Unlock()
}

// Logs a lifecycle line and keeps it in the crash log beside the gateway's own
func (c *gatewayClient) lifecycleLocked(line string) {
	c.pushLogLocked(line)
	recordParentLifecycle(line)
}

// Newest lines of the logs tail, at least one
func (c *gatewayClient) logTail(limit int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.logTailLocked(limit)
}

func (c *gatewayClient) logTailLocked(limit int) string {
	limit = max(1, limit)
	from := max(0, len(c.logs)-limit)
	return strings.Join(c.logs[from:], "\n")
}

// Socket of the current generation once it opened, starting or awaiting the transport as needed
func (c *gatewayClient) ensureAttached(ctx context.Context, method string) (*gatewayConn, error) {
	c.mu.Lock()
	if c.attachURL == "" {
		c.mu.Unlock()
		return nil, errors.New("gateway not running")
	}
	if c.conn == nil || c.conn.closed {
		c.startLocked()
		flight := c.bootstrapFlight
		explicit := attachURLEnv() != ""
		c.mu.Unlock()
		c.deliver()
		if !explicit && flight != nil {
			select {
			case <-flight:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.mu.Lock()
	}
	if conn := c.conn; conn != nil && !conn.open {
		c.mu.Unlock()
		select {
		case <-conn.settled:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if conn.dialErr != nil {
			return nil, conn.dialErr
		}
		c.mu.Lock()
	}
	conn := c.conn
	c.mu.Unlock()
	if conn == nil || !conn.open || conn.closed {
		return nil, fmt.Errorf("gateway not connected: %s", method)
	}
	return conn, nil
}

// Sends a request on a socket generation and waits for its answer, failing with the cause of a detach
func (c *gatewayClient) call(ctx context.Context, conn *gatewayConn, method string, params map[string]any, timeout time.Duration) (any, error) {
	if timeout <= 0 {
		timeout = c.timeout
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	call := &pendingCall{cancel: cancel}
	c.mu.Lock()
	if c.conn != conn || conn.link == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("gateway not connected: %s", method)
	}
	if c.pending == nil {
		c.pending = map[*pendingCall]struct{}{}
	}
	c.pending[call] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, call)
		c.mu.Unlock()
	}()
	callCtx, stop := context.WithTimeout(reqCtx, timeout)
	defer stop()
	if params == nil {
		params = map[string]any{}
	}
	value, err := conn.link.call(callCtx, method, params)
	if err != nil {
		if refusal, isRefusal := err.(*wireRefusal); isRefusal {
			return nil, rpcErrorFromFrame(refusal.member)
		}
		var gone *linkGoneError
		if errors.As(err, &gone) && ctx.Err() == nil {
			<-reqCtx.Done()
		}
		if cause := context.Cause(reqCtx); reqCtx.Err() != nil && ctx.Err() == nil && cause != nil {
			return nil, cause
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
			return nil, fmt.Errorf("request timed out after %ds: %s", int(math.Round(timeout.Seconds())), method)
		}
		return nil, err
	}
	c.deliverOpenRequests(value)
	return value, nil
}

// Typed refusal of a response frame's error member, the fallback message when it carries none
func rpcErrorFromFrame(member any) *rpcError {
	raw, _ := member.(map[string]any)
	out := &rpcError{Message: "Icebath RPC failed"}
	if message, isStr := raw["message"].(string); isStr && message != "" {
		out.Message = message
	}
	if code, isNum := raw["code"].(float64); isNum {
		out.Code, out.HasCode = int64(code), true
	}
	out.Data, _ = raw["data"].(map[string]any)
	return out
}

// Redelivers the server requests a resume answer reports still open, before its caller sees the answer
func (c *gatewayClient) deliverOpenRequests(value any) {
	result, _ := value.(map[string]any)
	open, isList := result["open_requests"].([]any)
	if !isList {
		return
	}
	c.locked(func() {
		for _, item := range open {
			entry, _ := item.(map[string]any)
			id, idStr := entry["id"].(string)
			method, methodStr := entry["method"].(string)
			if !idStr || !methodStr {
				continue
			}
			params, _ := entry["params"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			c.deliverRequestLocked(id, method, params, true)
		}
	})
}

// Request over the attached WebSocket, waiting for the transport to open
func (c *gatewayClient) requestOverWebSocket(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	conn, err := c.ensureAttached(ctx, method)
	if err != nil {
		return nil, err
	}
	return c.call(ctx, conn, method, params, timeout)
}

// Gateway request, reshaped for the canonical wire, restarting the transport when the attach URL rotated
func (c *gatewayClient) request(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	if attach := attachURLEnv(); attach != "" {
		c.locked(func() {
			if c.attachURL != attach {
				c.detachLocked(errors.New(tr(txt.LibTextGatewayAttachUrlChanged)))
				c.startLocked()
			}
		})
		return c.send(ctx, method, params, timeout)
	}
	var flight chan struct{}
	c.locked(func() {
		if c.bootstrapFlight == nil {
			c.startLocked()
		}
		flight = c.bootstrapFlight
	})
	if flight != nil {
		select {
		case <-flight:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	failure := c.bootstrapErr
	c.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	return c.send(ctx, method, params, timeout)
}

// One request on the socket, a session.create stamped as this window's and retried under its request id
func (c *gatewayClient) send(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	if method == tuigateway.CanonicalSessionCreate.Name() {
		return c.createCanonical(ctx, createRequest(params), timeout)
	}
	return c.requestOverWebSocket(ctx, method, params, timeout)
}

// session.create plus the launch tool progress pin as one outcome, a doubtful create retried under its request id
func (c *gatewayClient) createCanonical(ctx context.Context, prepared map[string]any, timeout time.Duration) (any, error) {
	toolProgress, err := launchToolProgress(c.env)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(prepared))
	for name := range prepared {
		if name != "request_id" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	pairs := make([][2]any, len(names))
	for i, name := range names {
		pairs[i] = [2]any{name, prepared[name]}
	}
	rawKey, err := json.Marshal(pairs)
	if err != nil {
		return nil, err
	}
	key := string(rawKey)
	c.mu.Lock()
	var retry any
	if waiting := c.unsettled[key]; len(waiting) > 0 {
		retry = waiting[0]
		if c.unsettled[key] = waiting[1:]; len(waiting) == 1 {
			delete(c.unsettled, key)
		}
	}
	c.mu.Unlock()
	params := prepared
	if retry != nil {
		params = withKeys(prepared, "request_id", retry)
	}
	acknowledged := false
	value, err := c.requestOverWebSocket(ctx, "session.create", params, timeout)
	if err == nil {
		acknowledged = true
		if toolProgress != "" {
			sid, _ := value.(map[string]any)["session_id"].(string)
			scope := "session"
			pin := tuigateway.SettingsSetParams{Key: "display.tool_progress", SessionID: &sid, Value: toolProgress, Scope: &scope}
			_, err = c.requestOverWebSocket(ctx, tuigateway.SettingsSet.Name(), wireParams(pin), 0)
		}
	}
	if err != nil {
		var refusal *rpcError
		if acknowledged || !errors.As(err, &refusal) {
			c.mu.Lock()
			if c.unsettled == nil {
				c.unsettled = map[string][]any{}
			}
			c.unsettled[key] = append(c.unsettled[key], params["request_id"])
			c.mu.Unlock()
		}
		return nil, err
	}
	return value, nil
}

// Detaches for good with no reconnect and every request failed, the shared gateway running on for other clients
func (c *gatewayClient) kill(reason string) {
	c.locked(func() {
		c.disposed = true
		c.localGeneration++
		c.clearReconnectLocked()
		c.reconnects = 0
		c.stopHeartbeatLocked()
		c.lifecycleLocked(fmt.Sprintf("[lifecycle] GatewayClient.kill reason=%s (detach only)", reason))
		c.closeGatewaySocketLocked()
		c.clearReadyTimerLocked()
		c.detachLocked(errors.New(tr(txt.LibTextGatewayClosed)))
	})
}

// Begins the gateway.ping keepalive on the current socket, dropping it after a deadline without inbound frames
func (c *gatewayClient) startHeartbeatLocked() {
	c.stopHeartbeatLocked()
	now := c.clock.Now()
	c.lastLiveness = now
	conn := c.conn
	if conn == nil {
		return
	}
	token := new(int)
	c.heartbeatToken = token
	c.lastPingAt, c.lastTickAt, c.previousLate, c.graceSent = time.Time{}, now, false, false
	c.armHeartbeatLocked(conn, token)
}

func (c *gatewayClient) armHeartbeatLocked(conn *gatewayConn, token *int) {
	c.heartbeatStop = c.clock.AfterFunc(heartbeatInterval, func() { c.heartbeatTick(conn, token) })
}

// Keepalive tick that fails after an unanswered deadline, probes once after a local stall, else pings
func (c *gatewayClient) heartbeatTick(conn *gatewayConn, token *int) {
	c.mu.Lock()
	if c.heartbeatToken != token {
		c.mu.Unlock()
		return
	}
	if c.conn != conn || conn.link == nil {
		c.armHeartbeatLocked(conn, token)
		c.mu.Unlock()
		return
	}
	now := c.clock.Now()
	late := float64(now.Sub(c.lastTickAt)) >= float64(heartbeatInterval)*lateTickIntervals
	stalled := late && !c.previousLate
	c.lastTickAt, c.previousLate = now, late
	unanswered := c.lastPingAt.After(c.lastLiveness)
	if !unanswered {
		c.graceSent = false
	}
	if unanswered && now.Sub(c.lastLiveness) >= heartbeatDeadline {
		if !stalled || c.graceSent {
			c.stopHeartbeatLocked()
			c.onHeartbeatFailureLocked()
			c.mu.Unlock()
			c.deliver()
			return
		}
		c.graceSent = true
	}
	c.pingSeq++
	id := fmt.Sprintf("heartbeat-%d", c.pingSeq)
	c.pings = append(c.pings, id)
	if len(c.pings) > maxOutstandingPings {
		c.pings = c.pings[1:]
	}
	c.lastPingAt = now
	c.armHeartbeatLocked(conn, token)
	link := conn.link
	c.mu.Unlock()
	if err := link.sendFrame(`{"jsonrpc":"2.0","id":"` + id + `","method":"gateway.ping","params":{}}`); err != nil {
		c.locked(func() {
			if c.heartbeatToken == token {
				c.stopHeartbeatLocked()
				c.onHeartbeatFailureLocked()
			}
		})
	}
}

func (c *gatewayClient) stopHeartbeatLocked() {
	c.pings = nil
	if c.heartbeatStop != nil {
		c.heartbeatStop()
	}
	c.heartbeatStop, c.heartbeatToken = nil, nil
}
