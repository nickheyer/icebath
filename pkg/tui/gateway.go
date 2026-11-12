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
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
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
	Params    map[string]any
}

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

// Grant of the local bootstrap, the private WebSocket target of the shared owner
type localGrant struct {
	URL        string
	Protocols  []string
	ProfileID  string
	InstanceID string
}

// Clock the client schedules its timers on, wall time in production
type gatewayClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

// Parsed leading integer of an env value as JavaScript parseInt reads it, ok false for NaN
func jsParseInt(raw string) (int, bool) {
	s := strings.TrimLeftFunc(raw, isJSSpace)
	sign := 1
	if s != "" && (s[0] == '+' || s[0] == '-') {
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	n, digits := 0, 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		if n < math.MaxInt32 {
			n = n*10 + int(s[digits]-'0')
		}
		digits++
	}
	return sign * n, digits > 0
}

// Milliseconds of an env value with its floor, the fallback for NaN or zero
func envMillis(name string, fallback, floor int) time.Duration {
	ms, ok := jsParseInt(os.Getenv(name))
	if !ok || ms == 0 {
		ms = fallback
	}
	return time.Duration(max(floor, ms)) * time.Millisecond
}

// Request timeout, ICEBATH_TUI_RPC_TIMEOUT_MS with a thirty second floor and two minutes by default
func requestTimeout() time.Duration { return envMillis("ICEBATH_TUI_RPC_TIMEOUT_MS", 120000, 30000) }

// Wait for gateway.ready, ICEBATH_TUI_STARTUP_TIMEOUT_MS with a five second floor and fifteen by default
func startupTimeout() time.Duration { return envMillis("ICEBATH_TUI_STARTUP_TIMEOUT_MS", 15000, 5000) }

// Explicit gateway WebSocket the TUI attaches to instead of the local owner, empty when unset
func attachURLEnv() string { return strings.TrimFunc(os.Getenv("ICEBATH_TUI_GATEWAY_URL"), isJSSpace) }

// Sidecar socket every event frame is mirrored to, empty when unset
func sidecarURLEnv() string { return strings.TrimFunc(os.Getenv("ICEBATH_TUI_SIDECAR_URL"), isJSSpace) }

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

// Grant of the in process bootstrap, its verdict text as the error
func localBootstrap(ctx context.Context, start, recover bool) (localGrant, error) {
	ctx, cancel := context.WithTimeout(ctx, bootstrapTimeout)
	defer cancel()
	obj, err := GatewayBootstrap(ctx, start, recover)
	if err != nil {
		return localGrant{}, errors.New(BootstrapVerdict(err))
	}
	plain, _ := pyjson.Plain(obj).(map[string]any)
	grant := localGrant{}
	grant.URL, _ = plain["url"].(string)
	grant.ProfileID, _ = plain["profile_id"].(string)
	grant.InstanceID, _ = plain["instance_id"].(string)
	protocols, _ := plain["protocols"].([]any)
	for _, p := range protocols {
		if s, isStr := p.(string); isStr {
			grant.Protocols = append(grant.Protocols, s)
		}
	}
	return grant, nil
}

// Socket on a gateway or sidecar URL with no frame size cap, as a browser WebSocket reads
func dialGatewaySocket(ctx context.Context, target string, protocols []string) (gateway.GatewaySocket, error) {
	return gateway.DialGatewaySocket(ctx, target, protocols, 0)
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

	sock   gateway.GatewaySocket
	link   *gateway.GatewayClient
	open   bool
	closed bool
}

// Closes the socket of a generation, its reader then reporting the close
func (g *gatewayConn) shut() {
	g.cancel()
	if g.link != nil {
		go g.link.Close()
	}
}

// Request in flight on a socket generation, failed with the cause the transport detached it with
type pendingCall struct{ cancel context.CancelCauseFunc }

// Hold of one session's canonical frames while its resume is in flight
type attachHold struct {
	sid    string
	frames []gatewayEventMsg
}

type describeFlight struct {
	done chan struct{}
	err  error
}

type unsettledCreate struct {
	key       string
	requestID any
}

// Shared gateway client with bootstrap, attach, readiness, heartbeat, reconnect and sidecar mirror, as the ui-tui one
type gatewayClient struct {
	bootstrap func(ctx context.Context, start, recover bool) (localGrant, error)
	dial      func(ctx context.Context, target string, protocols []string) (gateway.GatewaySocket, error)
	clock     gatewayClock
	env       func(string) string
	timeout   time.Duration

	mu         sync.Mutex
	conn       *gatewayConn
	sidecar    gateway.GatewaySocket
	sidecarGen int
	attachURL  string
	sidecarURL string
	logs       []string
	pending    map[*pendingCall]struct{}

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

	isCanonical     bool
	contract        *creationContract
	describedSkin   map[string]any
	capabilities    []string
	describing      *describeFlight
	hold            *attachHold
	unsettled       *unsettledCreate
	countsDeclines  bool
	clarifyBatchAck bool

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

// Client of the local shared gateway, started by start and subscribed by attach
func newGatewayClient() *gatewayClient {
	return &gatewayClient{bootstrap: localBootstrap, dial: dialGatewaySocket, clock: systemClock{}, env: launchEnv(), timeout: requestTimeout()}
}

// Whether the client attaches an explicit or granted WebSocket
func (c *gatewayClient) attached() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attachURL != ""
}

// Whether the client speaks the canonical wire, a local grant or a negotiated explicit attach
func (c *gatewayClient) canonical() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isCanonical
}

var _ turnGateway = (*gatewayClient)(nil)

// Whether the owner takes batched clarify prompts, advertised as clarify-batch-v1 by runtime.describe
func (c *gatewayClient) clarifyBatch() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isCanonical && slices.Contains(c.capabilities, "clarify-batch-v1")
}

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
		if ev.Payload["heartbeat"] == true && c.conn != nil && c.conn.open {
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

// Event of a gateway frame's params, its payload an object or empty
func wireEvent(params map[string]any) gatewayEventMsg {
	ev := gatewayEventMsg{Payload: map[string]any{}, Params: params}
	ev.Type, _ = params["type"].(string)
	ev.SessionID, _ = params["session_id"].(string)
	if payload, isDict := params["payload"].(map[string]any); isDict {
		ev.Payload = payload
	}
	return ev
}

// Event off the socket, stamped with the owner's execution epoch on a canonical gateway and held for a pending attach
func (c *gatewayClient) publishWireLocked(conn *gatewayConn, ev gatewayEventMsg) {
	if !c.isCanonical && ev.Type == "gateway.ready" && c.attachURL != "" && ev.Payload["session_authority"] == true {
		c.adoptCanonicalAttachLocked(conn, ev)
		return
	}
	if c.isCanonical && ev.Type == "gateway.ready" {
		if truthyJS(ev.Payload["heartbeat"]) && conn.open {
			c.startHeartbeatLocked()
		}
		return
	}
	if c.isCanonical {
		payload := canonicalEvent(ev).Payload
		stamp := func(name string) (any, bool) {
			if v, has := ev.Params[name]; has && v != nil {
				return v, true
			}
			v, has := payload[name]
			return v, has
		}
		out := make(map[string]any, len(payload)+2)
		for k, v := range payload {
			out[k] = v
		}
		if epoch, has := stamp("authority_epoch"); has {
			out["execution_epoch"] = jsString(epoch)
		}
		if generation, has := stamp("execution_generation"); has {
			out["execution_generation"] = generation
		}
		ev.Payload = out
		if c.hold != nil && ev.Params["session_id"] == c.hold.sid {
			c.hold.frames = append(c.hold.frames, ev)
			return
		}
	}
	c.publishLocked(ev)
}

// Negotiates the canonical wire on an explicit listener whose ready advertises a session authority
func (c *gatewayClient) adoptCanonicalAttachLocked(conn *gatewayConn, ev gatewayEventMsg) {
	c.isCanonical = true
	if truthyJS(ev.Payload["heartbeat"]) && conn.open {
		c.startHeartbeatLocked()
	}
	ready := map[string]any{}
	if skin, has := ev.Payload["skin"]; has {
		ready["skin"] = skin
	}
	c.describeThenReadyLocked(conn, ready)
}

// Text of a failure as JavaScript String renders an Error
func jsErrorText(err error) string {
	var refusal *rpcError
	if errors.As(err, &refusal) {
		return "JsonRpcGatewayError: " + refusal.Message
	}
	return "Error: " + err.Error()
}

// Publishes readiness after runtime.describe answers on this socket, a failed describe dropping it onto the backoff path
func (c *gatewayClient) describeThenReadyLocked(conn *gatewayConn, payload map[string]any) {
	c.describing = nil
	c.capabilities = nil
	go func() {
		err := c.describeRuntime(context.Background())
		c.locked(func() {
			if err == nil {
				ready := payload
				if _, has := payload["skin"]; !has && c.describedSkin != nil {
					ready = withKeys(payload, "skin", c.describedSkin)
				}
				if c.conn == conn {
					c.publishLocked(gatewayEventMsg{Type: "gateway.ready", Payload: ready})
				}
				return
			}
			c.publishLocked(gatewayEventMsg{Type: "gateway.start_timeout", Payload: map[string]any{
				"python": "runtime.describe", "cwd": "", "stderr_tail": jsErrorText(err)}})
			if c.conn == conn {
				c.closeGatewaySocketLocked()
				c.handleTransportExitLocked(1, jsErrorText(err))
			}
		})
	}()
}

// Reads the owner's session.create contract, capabilities and skin once per socket, a failure not cached
func (c *gatewayClient) describeRuntime(ctx context.Context) error {
	c.mu.Lock()
	flight := c.describing
	owner := flight == nil
	if owner {
		flight = &describeFlight{done: make(chan struct{})}
		c.describing = flight
	}
	c.mu.Unlock()
	if owner {
		value, err := c.requestOverWebSocket(context.Background(), "runtime.describe", map[string]any{}, 0)
		c.mu.Lock()
		if err == nil {
			description, _ := value.(map[string]any)
			c.contract = parseCreationContract(description["session_create"])
			c.capabilities = nil
			if list, isList := description["capabilities"].([]any); isList {
				for _, item := range list {
					if s, isStr := item.(string); isStr {
						c.capabilities = append(c.capabilities, s)
					}
				}
			}
			c.describedSkin, _ = description["skin"].(map[string]any)
		} else if c.describing == flight {
			c.describing = nil
		}
		flight.err = err
		c.mu.Unlock()
		close(flight.done)
	}
	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Creation contract of a runtime.describe answer, nil when it carries none
func parseCreationContract(v any) *creationContract {
	raw, isDict := v.(map[string]any)
	if !isDict {
		return nil
	}
	strs := func(list any) []string {
		var out []string
		items, _ := list.([]any)
		for _, item := range items {
			if s, isStr := item.(string); isStr {
				out = append(out, s)
			}
		}
		return out
	}
	return &creationContract{Sources: strs(raw["sources"]), Parameters: strs(raw["parameters"])}
}

func (c *gatewayClient) clearReadyTimerLocked() {
	if c.readyStop != nil {
		c.readyStop()
		c.readyStop, c.readyToken = nil, nil
	}
}

func (c *gatewayClient) closeSidecarLocked() {
	c.sidecarGen++
	if c.sidecar != nil {
		sock := c.sidecar
		c.sidecar = nil
		go sock.Close()
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
	c.publishLocked(gatewayEventMsg{Type: "gateway.reconnecting", Payload: map[string]any{"attempt": float64(c.reconnects), "delay_ms": float64(ms)}})
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
	c.detachLocked(errors.New(tr("libText.gateway.restarting")))
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
			c.publishLocked(gatewayEventMsg{Type: "gateway.start_timeout", Payload: map[string]any{"cwd": cwd, "python": python, "stderr_tail": tail}})
		})
	})
}

// Fails the transport's requests, schedules the reconnect and tells the subscriber the exit code
func (c *gatewayClient) handleTransportExitLocked(code int, reason string) {
	c.clearReadyTimerLocked()
	c.ready = false
	c.closeSidecarLocked()
	reasonText := reason
	if reasonText == "" {
		reasonText = "none"
	}
	c.lifecycleLocked(fmt.Sprintf("[lifecycle] transport exit code=%d reason=%s", code, reasonText))
	if reason == "" {
		reason = tr("libText.gateway.exitedWithCode", code)
	}
	c.detachLocked(errors.New(reason))
	c.scheduleReconnectLocked()
	if c.sink != nil {
		c.outbox = append(c.outbox, gatewayExitMsg{Code: code})
		return
	}
	c.pendingExit = &code
}

// Opens the sidecar mirror socket when ICEBATH_TUI_SIDECAR_URL names one
func (c *gatewayClient) connectSidecarMirrorLocked() {
	c.closeSidecarLocked()
	target := c.sidecarURL
	if target == "" {
		return
	}
	if !constructibleWSURL(target) {
		c.pushLogLocked(fmt.Sprintf("[sidecar] failed to connect %s (constructor error)", redactURL(target)))
		return
	}
	gen := c.sidecarGen
	go func() {
		sock, err := c.dial(context.Background(), target, nil)
		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			c.pushLogLocked("[sidecar] mirror connection error")
			return
		}
		if c.sidecarGen != gen {
			go sock.Close()
			return
		}
		c.sidecar = sock
		go func() {
			for {
				if _, err := sock.Recv(); err != nil {
					c.mu.Lock()
					if c.sidecar == sock {
						c.sidecar = nil
					}
					c.mu.Unlock()
					return
				}
			}
		}()
	}()
}

// Sends one raw event frame to the sidecar when it is open
func (c *gatewayClient) mirrorLocked(raw string) {
	if sock := c.sidecar; sock != nil && sock.Open() {
		_ = sock.Send(raw)
	}
}

// Starts holding a session's canonical frames while its resume is in flight, nil off the canonical wire
func (c *gatewayClient) holdAttach(sessionID string) *attachHold {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isCanonical {
		return nil
	}
	c.hold = &attachHold{sid: sessionID}
	return c.hold
}

// Ends a hold, replaying only the frames past the applied snapshot's fence, or all of them without one
func (c *gatewayClient) settleAttach(hold *attachHold, snapshot map[string]any) {
	c.locked(func() {
		if hold == nil || c.hold != hold {
			return
		}
		c.hold = nil
		for _, ev := range hold.frames {
			if snapshot == nil {
				c.publishLocked(ev)
				continue
			}
			fence := float64(-1)
			if last, has := snapshot["last_sequence"]; has && last != nil {
				fence = jsNumberOf(last)
			}
			seq, isNum := ev.Params["seq"].(float64)
			epoch, hasEpoch := ev.Params["replay_epoch"]
			want, hasWant := snapshot["replay_epoch"]
			if hasEpoch == hasWant && jsStrictEqual(epoch, want) && isNum && seq > fence {
				c.publishLocked(ev)
			}
		}
	})
}

// Number of a JSON value as JavaScript Number converts it, NaN when it has none
func jsNumberOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		if f, ok := jsNumber(t); ok {
			return f
		}
	case nil:
		return 0
	}
	return math.NaN()
}

// Whether two decoded JSON values are equal under JavaScript ===, compound values never
func jsStrictEqual(a, b any) bool {
	switch a.(type) {
	case nil, bool, float64, string:
		return a == b
	}
	return false
}

// Publishes a resumed session's shared prompts and redelivers its open server requests
func (c *gatewayClient) hydrateSharedPrompts(result map[string]any) {
	c.locked(func() {
		if !c.isCanonical {
			return
		}
		sid, _ := result["session_id"].(string)
		prompts, _ := result["prompts"].([]any)
		for _, item := range prompts {
			prompt, _ := item.(map[string]any)
			payload := withKeys(prompt, "execution_epoch", jsString(result["authority_epoch"]))
			c.publishLocalEventLocked(gatewayEventMsg{Type: jsString(prompt["kind"]) + ".request", SessionID: sid, Payload: payload})
		}
		open, _ := result["open_requests"].([]any)
		for _, item := range open {
			req, _ := item.(map[string]any)
			params, _ := req["params"].(map[string]any)
			if params == nil {
				params = map[string]any{}
			}
			method, _ := req["method"].(string)
			c.deliverRequestLocked(req["id"], method, params, true)
		}
	})
}

// Publishes a client side event and mirrors it to the sidecar as an event frame
func (c *gatewayClient) publishLocalEvent(ev gatewayEventMsg) {
	c.locked(func() { c.publishLocalEventLocked(ev) })
}

func (c *gatewayClient) publishLocalEventLocked(ev gatewayEventMsg) {
	params := map[string]any{"type": ev.Type, "payload": ev.Payload}
	if ev.SessionID != "" {
		params["session_id"] = ev.SessionID
	}
	if raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "event", "params": params}); err == nil {
		c.mirrorLocked(string(raw))
	}
	ev.Params = params
	c.publishLocked(ev)
}

// Logs a frame the client could not read and publishes gateway.protocol_error
func (c *gatewayClient) protocolErrorLocked(what, text, emptyLabel string) {
	preview := sliceUnits(strings.TrimFunc(text, isJSSpace), maxLogPreview)
	if preview == "" {
		preview = emptyLabel
	}
	c.pushLogLocked("[protocol] " + what + ": " + preview)
	c.publishLocked(gatewayEventMsg{Type: "gateway.protocol_error", Payload: map[string]any{"preview": preview}})
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
				c.isCanonical = true
				mode := "discover"
				if start {
					mode = "start"
				} else if recover {
					mode = "recover"
				}
				c.lifecycleLocked(fmt.Sprintf("[lifecycle] attached shared gateway %s url=%s profile=%s instance=%s", mode, redactURL(grant.URL), grant.ProfileID, grant.InstanceID))
				c.startAttachedGatewayLocked(grant.URL, grant.Protocols)
				return
			}
			c.bootstrapErr = err
			c.clearReadyTimerLocked()
			c.publishLocked(gatewayEventMsg{Type: "gateway.start_timeout", Payload: map[string]any{"python": "gateway ensure", "cwd": "", "stderr_tail": err.Error()}})
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
		c.handleTransportExitLocked(1, tr("libText.gateway.websocketStartupFailed"))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &gatewayConn{url: target, settled: make(chan struct{}), cancel: cancel}
	c.conn = conn
	c.stopHeartbeatLocked()
	c.lastLiveness = c.clock.Now()
	c.countsDeclines, c.clarifyBatchAck = false, false
	go func() {
		sock, err := c.dial(ctx, target, protocols)
		c.locked(func() {
			defer close(conn.settled)
			if err != nil {
				c.pushLogLocked("[startup] gateway websocket connect error")
				conn.dialErr = errors.New(tr("libText.gateway.websocketConnectionFailed"))
				c.transportErrorLocked(conn)
				c.onSocketCloseLocked(conn, closeAbnormal)
				return
			}
			if c.conn != conn || ctx.Err() != nil {
				conn.dialErr = errors.New(tr("libText.gateway.websocketConnectionFailed"))
				go sock.Close()
				c.pushLogLocked(fmt.Sprintf("[lifecycle] stale websocket close ignored code=%d", closeAbnormal))
				return
			}
			link := gateway.NewGatewayClient(sock)
			link.SetFrameHooks(gateway.FrameHooks{
				Inbound:   func(raw string, frame *pyjson.Object) { c.inbound(conn, raw, frame) },
				Malformed: func(raw string) { c.malformed(conn, raw) },
			})
			conn.sock, conn.link, conn.open = sock, link.Start(), true
			go c.pump(conn)
			c.connectSidecarMirrorLocked()
			if c.isCanonical {
				c.describeThenReadyLocked(conn, map[string]any{})
			}
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
	c.publishLocked(gatewayEventMsg{Type: "gateway.stderr", Payload: map[string]any{"line": line}})
}

// Liveness, heartbeat acknowledgement and sidecar mirror of every frame the current socket reads
func (c *gatewayClient) inbound(conn *gatewayConn, raw string, frame *pyjson.Object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != conn {
		return
	}
	c.lastLiveness = c.clock.Now()
	if id, isStr := frame.Vals["id"].(string); isStr {
		if i := slices.Index(c.pings, id); i >= 0 {
			c.pings = slices.Delete(c.pings, i, i+1)
		}
	}
	if method, _ := frame.Vals["method"].(string); method == "event" {
		c.mirrorLocked(raw)
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
	q := conn.link.EventQueue()
	for {
		item, err := q.Get(context.Background())
		if err != nil {
			return
		}
		if _, isErr := item.(error); isErr {
			code, clean := closeCode(conn.link.ReadError())
			c.locked(func() {
				conn.closed = true
				if !clean {
					c.transportErrorLocked(conn)
				}
				c.onSocketCloseLocked(conn, code)
			})
			return
		}
		c.route(conn, item)
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
	reason := tr("libText.gateway.websocketClosed")
	if code != 0 {
		reason = tr("libText.gateway.websocketClosedWithCode", code)
	}
	c.handleTransportExitLocked(code, reason)
}

// Routes one frame of the current socket, an event to the wire publisher and a method with an id as a server request
func (c *gatewayClient) route(conn *gatewayConn, item any) {
	decoded, err := jsValue(item)
	frame, isDict := decoded.(map[string]any)
	c.locked(func() {
		if c.conn != conn {
			return
		}
		if err != nil || !isDict {
			c.protocolErrorLocked("malformed websocket frame", fmt.Sprint(item), "(empty frame)")
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

// Tells the backend once per ready that this client answers server requests, recording what it counts
func (c *gatewayClient) advertiseCapabilitiesLocked(conn *gatewayConn) {
	go func() {
		value, err := c.call(context.Background(), conn, "client.capabilities", map[string]any{"server_requests": true}, 0)
		if err != nil {
			return
		}
		result, _ := value.(map[string]any)
		c.mu.Lock()
		if c.conn == conn {
			c.countsDeclines = result["declines_not_shown"] == true
			c.clarifyBatchAck = result["canonical_clarify_batch"] == true
		}
		c.mu.Unlock()
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
		_ = conn.link.SendFrame(raw)
	}
}

// Answers the request with a result
func (r *serverRequest) respond(result any) { r.send("result", result) }

// Answers the request with an error
func (r *serverRequest) fail(code int, message string) {
	r.send("error", map[string]any{"code": code, "message": message})
}

// Declines a request this client did not show, answered only to a backend that counts declines
func (r *serverRequest) decline(message string) {
	r.client.mu.Lock()
	counts := r.client.countsDeclines
	r.client.mu.Unlock()
	if counts {
		r.send("error", map[string]any{"code": jsonRPCSessionNotShown, "message": message})
	}
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
	c.sidecarURL = sidecarURLEnv()
	c.resetStartupStateLocked()
	c.closeGatewaySocketLocked()
	c.closeSidecarLocked()
	if c.attachURL != "" {
		c.isCanonical = false
		c.contract = nil
		c.capabilities = nil
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
	result, err := conn.link.RPCTimeout(callCtx, method, params, -1)
	if err != nil {
		if refusal, isRefusal := err.(*gateway.GatewayRPCError); isRefusal {
			return nil, rpcErrorFromFrame(refusal.Raw)
		}
		var gone *gateway.GatewayClientError
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
	value, err := jsValue(result)
	if err != nil {
		return nil, err
	}
	c.deliverOpenRequests(value)
	return value, nil
}

// Typed refusal of a response frame's error member, the fallback message when it carries none
func rpcErrorFromFrame(member any) *rpcError {
	decoded, _ := jsValue(member)
	raw, _ := decoded.(map[string]any)
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
		var canonical bool
		c.locked(func() {
			if c.attachURL != attach {
				c.detachLocked(errors.New(tr("libText.gateway.attachUrlChanged")))
				c.startLocked()
			}
			canonical = c.isCanonical
		})
		if canonical {
			return c.requestCanonical(ctx, method, params, timeout)
		}
		return c.requestOverWebSocket(ctx, method, params, timeout)
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
	return c.requestCanonical(ctx, method, params, timeout)
}

// One request through the canonical wire adapter, describing the runtime before the first session.create
func (c *gatewayClient) requestCanonical(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	c.mu.Lock()
	contract := c.contract
	c.mu.Unlock()
	if method == "session.create" && contract == nil {
		if err := c.describeRuntime(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		contract = c.contract
		c.mu.Unlock()
	}
	wireMethod, wireParams, err := canonicalRequest(method, params, contract)
	if err != nil {
		return nil, err
	}
	if wireMethod == "session.create" {
		return c.createCanonical(ctx, wireParams, timeout)
	}
	value, err := c.requestOverWebSocket(ctx, wireMethod, wireParams, timeout)
	if err != nil {
		return nil, err
	}
	return canonicalResult(method, value, wireParams), nil
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
	retry := c.unsettled
	c.unsettled = nil
	c.mu.Unlock()
	params := prepared
	if retry != nil && retry.key == key {
		params = withKeys(prepared, "request_id", retry.requestID)
	}
	acknowledged := false
	value, err := c.requestOverWebSocket(ctx, "session.create", params, timeout)
	if err == nil {
		acknowledged = true
		if toolProgress != "" {
			sid, _ := value.(map[string]any)["session_id"]
			_, err = c.requestOverWebSocket(ctx, "config.set", map[string]any{"key": "verbose", "session_id": sid, "value": toolProgress}, 0)
		}
	}
	if err != nil {
		var refusal *rpcError
		if acknowledged || !errors.As(err, &refusal) {
			c.mu.Lock()
			c.unsettled = &unsettledCreate{key: key, requestID: params["request_id"]}
			c.mu.Unlock()
		}
		return nil, err
	}
	return canonicalResult("session.create", value, params), nil
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
		c.closeSidecarLocked()
		c.clearReadyTimerLocked()
		c.detachLocked(errors.New(tr("libText.gateway.closed")))
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
	if err := link.SendFrame(`{"jsonrpc":"2.0","id":"` + id + `","method":"gateway.ping","params":{}}`); err != nil {
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

// Value in the JavaScript data model the ui-tui client decoded, objects as maps and numbers as float64
func jsValue(v any) (any, error) {
	raw, err := pyjson.Dumps(v, pyjson.SortKeys(false))
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}
