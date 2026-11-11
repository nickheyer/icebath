package gateway

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const GatewayWSTicketPrefix = "icebath-gateway-ticket."

// Per request answer budget, an RPC passing a negative timeout waits without a deadline
const DefaultRPCTimeout = 30 * time.Second

const ResumeKeyMismatch = "--api-key on resume must be the key this session was launched with " +
	"(a session launched without --api-key keeps its profile credentials). " +
	"A session never switches keys: start a new one (without --resume) to use a different key."

const CompressRPCTimeout = 660 * time.Second

// Stall bound and overflow caps of the event reader, variables so tests can shrink them
var (
	EventStallTimeout   = 30 * time.Second
	EventOverflowFrames = 16384
	EventOverflowBytes  = 8 * 1024 * 1024
)

const eventQueueSize = 4096

const GatewayDisconnected = "Gateway disconnected; turn outcome is unknown. " +
	"Resume the printed session ID to check whether it completed or was interrupted; do not resend the work."

const GatewayNotSent = "Gateway disconnected before the request was sent; nothing was sent."

const unknownTicketParams = "PermissionError: invalid bootstrap parameters"

// WebSocket URL and subprotocols for dialing an owner with a session ticket
func GatewayWSTarget(endpoint *GatewayEndpoint, ticket string) (string, []string) {
	u := strings.ReplaceAll(strings.ReplaceAll(endpoint.APIOrigin, "https:", "wss:"), "http:", "ws:") + "/api/ws"
	return u, []string{CanonicalGatewayProtocol, GatewayWSTicketPrefix + ticket}
}

type GatewayClientError struct{ Msg string }

func (e *GatewayClientError) Error() string { return e.Msg }

// Received owner refusal with its bounded reason, integer code and scalar only data
type GatewayRPCError struct {
	Msg  string
	Code *int64
	Data map[string]any
}

func (e *GatewayRPCError) Error() string { return e.Msg }

// Lets a refusal match a GatewayClientError target like the Python subclass
func (e *GatewayRPCError) As(target any) bool {
	if t, ok := target.(**GatewayClientError); ok {
		*t = &GatewayClientError{Msg: e.Msg}
		return true
	}
	return false
}

// No ready owner to dial, State the ensure verdict and nothing submitted
type GatewayUnavailableError struct {
	Msg, State, ReasonCode string
}

func (e *GatewayUnavailableError) Error() string { return e.Msg }

// Lets an unavailable owner match a GatewayClientError target like the Python subclass
func (e *GatewayUnavailableError) As(target any) bool {
	if t, ok := target.(**GatewayClientError); ok {
		*t = &GatewayClientError{Msg: e.Msg}
		return true
	}
	return false
}

// Reason code when it is a short alphanumeric or underscore string, else empty
func boundedReason(value any) string {
	s, ok := value.(string)
	if !ok || utf8.RuneCountInString(s) > 80 {
		return ""
	}
	stripped := strings.ReplaceAll(s, "_", "")
	if stripped == "" {
		return ""
	}
	for _, r := range stripped {
		if !pystr.IsAlnum(r) {
			return ""
		}
	}
	return s
}

// At most sixteen short keys whose values are short scalars or short lists of them
func boundedErrorData(data any) map[string]any {
	out := map[string]any{}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return out
	}
	scalar := func(v any) bool {
		switch x := v.(type) {
		case nil, bool, int, int64, *big.Int:
			return true
		case string:
			return utf8.RuneCountInString(x) <= 200
		}
		return false
	}
	for _, key := range obj.Keys {
		value := obj.Vals[key]
		if len(out) >= 16 || utf8.RuneCountInString(key) > 64 {
			continue
		}
		if scalar(value) {
			out[key] = value
		} else if list, ok := value.([]any); ok && len(list) <= 16 {
			all := true
			for _, item := range list {
				all = all && scalar(item)
			}
			if all {
				out[key] = append([]any(nil), list...)
			}
		}
	}
	return out
}

// Refusal for one received error member keeping only bounded reason codes
func RPCError(errMember any) *GatewayRPCError {
	obj, _ := errMember.(*pyjson.Object)
	if obj == nil {
		obj = pyjson.NewObject()
	}
	data := boundedErrorData(obj.Vals["data"])
	message, ok := obj.Get("message")
	if !ok {
		message = "request_failed"
	}
	reason := boundedReason(message)
	if reason == "" {
		reason = boundedReason(data["reason"])
	}
	if reason == "" {
		reason = "request_failed"
	}
	out := &GatewayRPCError{Msg: reason, Data: data}
	switch c := obj.Vals["code"].(type) {
	case int64:
		out.Code = &c
	case int:
		v := int64(c)
		out.Code = &v
	}
	return out
}

type heldEvent struct {
	size int
	item any
}

// Bounded event queue plus an ordered overflow behind it refilled on every get
type EventQueue struct {
	mu            sync.Mutex
	maxsize       int
	items         []any
	Overflow      []heldEvent
	OverflowBytes int
	moved         chan struct{}
	ready         chan struct{}
}

func NewEventQueue(maxsize int) *EventQueue {
	return &EventQueue{maxsize: maxsize, moved: make(chan struct{}), ready: make(chan struct{})}
}

// Wakes every waiter on a channel and arms a fresh one, the caller holding the lock
func wake(ch *chan struct{}) {
	close(*ch)
	*ch = make(chan struct{})
}

func (q *EventQueue) Maxsize() int { return q.maxsize }

func (q *EventQueue) QSize() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *EventQueue) Full() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.maxsize > 0 && len(q.items) >= q.maxsize
}

func (q *EventQueue) Empty() bool { return q.QSize() == 0 }

func (q *EventQueue) OverflowLen() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.Overflow)
}

// Oldest item with one overflow item moved into the queue, false when the queue is empty
func (q *EventQueue) GetNowait() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	if len(q.Overflow) > 0 {
		held := q.Overflow[0]
		q.Overflow = q.Overflow[1:]
		q.OverflowBytes -= held.size
		q.items = append(q.items, held.item)
	}
	wake(&q.moved)
	return item, true
}

// Oldest item, waiting until one arrives or the context ends
func (q *EventQueue) Get(ctx context.Context) (any, error) {
	for {
		if item, ok := q.GetNowait(); ok {
			return item, nil
		}
		q.mu.Lock()
		if len(q.items) > 0 {
			q.mu.Unlock()
			continue
		}
		ready := q.ready
		q.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Adds an item to the queue, false when the queue is full
func (q *EventQueue) PutNowait(item any) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.putLocked(item)
}

func (q *EventQueue) putLocked(item any) bool {
	if q.maxsize > 0 && len(q.items) >= q.maxsize {
		return false
	}
	q.items = append(q.items, item)
	wake(&q.ready)
	return true
}

// Queues an item behind everything already read without ever dropping it
func (q *EventQueue) Hold(item any, size int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.Overflow) == 0 && q.putLocked(item) {
		return
	}
	q.Overflow = append(q.Overflow, heldEvent{size, item})
	q.OverflowBytes += size
}

// Signals the reader that an item left the overflow or an RPC started waiting
func (q *EventQueue) setMoved() {
	q.mu.Lock()
	wake(&q.moved)
	q.mu.Unlock()
}

// Queues a local item behind every frame read so far, never dropped
func PutBehind(events *EventQueue, item any) { events.Hold(item, 0) }

// Socket a client reads frames from and writes requests to
type GatewaySocket interface {
	Recv() (raw string, err error)
	Send(data string) error
	Open() bool
	Subprotocol() string
	Close() error
}

type rpcAnswer struct {
	result any
	err    error
}

// One attached owner connection multiplexing RPC answers and the session event stream
type GatewayClient struct {
	socket   GatewaySocket
	Events   *EventQueue
	mu       sync.Mutex
	writeMu  sync.Mutex
	pending  map[int64]chan rpcAnswer
	sequence int64
	readDone chan struct{}
	started  bool
}

func NewGatewayClient(socket GatewaySocket) *GatewayClient {
	return &GatewayClient{socket: socket, Events: NewEventQueue(eventQueueSize), pending: map[int64]chan rpcAnswer{}, readDone: make(chan struct{})}
}

// Whether the reader keeps reading past a full queue for an awaited answer within the overflow caps
func (c *GatewayClient) readOn() bool {
	q := c.Events
	q.mu.Lock()
	n, b := len(q.Overflow), q.OverflowBytes
	q.mu.Unlock()
	if n == 0 {
		return true
	}
	if n >= EventOverflowFrames || b >= EventOverflowBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) > 0
}

// Holds one event frame and waits for the consumer when the reader may not read on
func (c *GatewayClient) deliver(frame any, size int) error {
	c.Events.Hold(frame, size)
	if c.readOn() {
		return nil
	}
	deadline := time.NewTimer(EventStallTimeout)
	defer deadline.Stop()
	for !c.readOn() {
		c.Events.mu.Lock()
		moved := c.Events.moved
		c.Events.mu.Unlock()
		if c.readOn() {
			return nil
		}
		select {
		case <-moved:
		case <-deadline.C:
			return os.ErrDeadlineExceeded
		}
	}
	return nil
}

// Starts the reader goroutine, the async context entry
func (c *GatewayClient) Start() *GatewayClient {
	c.mu.Lock()
	if !c.started {
		c.started = true
		go c.Read()
	}
	c.mu.Unlock()
	return c
}

// Closes the socket and waits for the reader to finish failing every pending answer
func (c *GatewayClient) Close() error {
	err := c.socket.Close()
	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if started {
		<-c.readDone
	}
	return err
}

// Event frames read off the socket, the client.events queue of the thin client
func (c *GatewayClient) EventQueue() *EventQueue { return c.Events }

// Requests still waiting for an answer
func (c *GatewayClient) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

// Writes one raw frame on the socket, the websocket.send of a server request reply
func (c *GatewayClient) SendFrame(text string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.socket.Send(text)
}

// Pending answer key of a frame id, matching integral floats and booleans as Python dict keys do
func frameID(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case int:
		return int64(x), true
	case float64:
		if x == float64(int64(x)) {
			return int64(x), true
		}
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Reads frames until the socket ends, answering RPCs and queueing events, then fails every waiter
func (c *GatewayClient) Read() {
	defer close(c.readDone)
	defer func() {
		failure := &GatewayClientError{Msg: GatewayDisconnected}
		c.mu.Lock()
		for id, ch := range c.pending {
			select {
			case ch <- rpcAnswer{err: failure}:
			default:
			}
			delete(c.pending, id)
		}
		c.mu.Unlock()
		c.Events.mu.Lock()
		if c.Events.maxsize > 0 && len(c.Events.items) >= c.Events.maxsize && len(c.Events.Overflow) == 0 {
			c.Events.items = c.Events.items[1:]
		}
		c.Events.mu.Unlock()
		c.Events.Hold(failure, 0)
	}()
	for {
		raw, err := c.socket.Recv()
		if err != nil {
			return
		}
		decoded, err := pyjson.Loads(raw)
		if err != nil {
			return
		}
		frame, ok := decoded.(*pyjson.Object)
		if !ok {
			return
		}
		var waiter chan rpcAnswer
		if idv, has := frame.Get("id"); has {
			if id, ok := frameID(idv); ok {
				c.mu.Lock()
				waiter = c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
			}
		}
		if waiter != nil {
			if errMember, has := frame.Get("error"); has {
				waiter <- rpcAnswer{err: RPCError(errMember)}
			} else if result, has := frame.Get("result"); has {
				waiter <- rpcAnswer{result: result}
			} else {
				waiter <- rpcAnswer{result: pyjson.NewObject()}
			}
		} else if _, has := frame.Get("method"); has {
			if c.deliver(frame, utf8.RuneCountInString(raw)) != nil {
				return
			}
		}
	}
}

// One request answered as a plain map, waiting the default budget
func (c *GatewayClient) RPC(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	result, err := c.RPCTimeout(ctx, method, params, 0)
	if err != nil {
		return nil, err
	}
	plain, ok := pyjson.Plain(result).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gateway %s answered %T, not an object", method, result)
	}
	return plain, nil
}

// One request and its raw answer, timeout zero the default budget and negative no deadline
func (c *GatewayClient) RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	select {
	case <-c.readDone:
		return nil, &GatewayClientError{Msg: GatewayNotSent}
	default:
	}
	if !c.socket.Open() {
		return nil, &GatewayClientError{Msg: GatewayNotSent}
	}
	answer := make(chan rpcAnswer, 1)
	c.mu.Lock()
	c.sequence++
	rid := c.sequence
	c.pending[rid] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, rid)
		c.mu.Unlock()
	}()
	c.Events.setMoved()
	if params == nil {
		params = map[string]any{}
	}
	request := pyjson.ObjectOf("jsonrpc", "2.0", "id", rid, "method", method, "params", params)
	text, err := pyjson.Dumps(request)
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	err = c.socket.Send(text)
	c.writeMu.Unlock()
	if err != nil {
		return nil, &GatewayClientError{Msg: GatewayDisconnected}
	}
	if timeout == 0 {
		timeout = DefaultRPCTimeout
	}
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case a := <-answer:
		return a.result, a.err
	case <-deadline:
		return nil, os.ErrDeadlineExceeded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type refuseRestartWaitKey struct{}

// Context whose gateway connections never attach to an owner in its restart after turn wait
func OutsideRestartWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, refuseRestartWaitKey{}, true)
}

func refusesRestartWait(ctx context.Context) bool {
	v, _ := ctx.Value(refuseRestartWaitKey{}).(bool)
	return v
}

// Request line for one session ticket mint over the control socket
func sessionTicketRequest(params *pyjson.Object) ([]byte, error) {
	text, err := pyjson.Dumps(pyjson.ObjectOf("protocol", 1, "id", 1, "verb", "session-ticket", "params", params))
	if err != nil {
		return nil, err
	}
	return []byte(text + "\n"), nil
}

// Session ticket minted by the owner control socket for an endpoint, checked against its identity
func sessionTicket(home string, endpoint *GatewayEndpoint, purpose, scope, subject string, refuseRestart bool) (string, error) {
	home = ControlHomeFor(home, endpoint)
	if purpose == "" {
		purpose = "interactive"
	}
	params := pyjson.ObjectOf("profile_id", endpoint.ProfileID, "instance_id", endpoint.InstanceID, "purpose", purpose)
	if scope != "" {
		params.Set("scope", scope)
	}
	if subject != "" {
		params.Set("subject", subject)
	}
	if refuseRestart {
		params.Set("restart_wait", "refuse")
	}
	request, err := sessionTicketRequest(params)
	if err != nil {
		return "", err
	}
	answer, err := privateControlQuery(home, request, 5*time.Second)
	if err != nil {
		return "", err
	}
	if refuseRestart {
		line, _, _ := strings.Cut(string(answer), "\n")
		refused, err := pyjson.Loads(line)
		if err != nil {
			return "", err
		}
		if obj, ok := refused.(*pyjson.Object); ok && obj.Vals["error"] == unknownTicketParams {
			params.Delete("restart_wait")
			if request, err = sessionTicketRequest(params); err != nil {
				return "", err
			}
			if answer, err = privateControlQuery(home, request, 5*time.Second); err != nil {
				return "", err
			}
		}
	}
	grant, err := identifyResponse(answer)
	if err != nil {
		return "", err
	}
	ticket, _ := grant.Vals["ticket"].(string)
	if grant.Vals["profile_id"] != endpoint.ProfileID || grant.Vals["instance_id"] != endpoint.InstanceID ||
		!pyjson.Equal(grant.Vals["runtime_protocol"], int64(1)) || ticket == "" {
		return "", &GatewayClientError{Msg: "Gateway bootstrap identity changed; retry launch"}
	}
	return ticket, nil
}

// Socket over a gorilla WebSocket connection with its open state tracked
type wsSocket struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	closed bool
}

func (s *wsSocket) Recv() (string, error) {
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		return "", err
	}
	return string(data), nil
}

func (s *wsSocket) Send(data string) error {
	return s.conn.WriteMessage(websocket.TextMessage, []byte(data))
}

func (s *wsSocket) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *wsSocket) Subprotocol() string { return s.conn.Subprotocol() }

func (s *wsSocket) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.conn.Close()
}

// Options of one gateway attach, the zero value ensuring this home's owner
type ConnectGatewayOptions struct {
	Endpoint *GatewayEndpoint
	Subject  string
	MaxSize  int64
}

// Attaches to this home's owner, starting it when needed
func ConnectGateway(ctx context.Context) (*GatewayClient, error) {
	return ConnectGatewayWith(ctx, ConnectGatewayOptions{})
}

// Attaches to the given endpoint, the explicit TUI gateway URL or this home's ensured owner
func ConnectGatewayWith(ctx context.Context, opts ConnectGatewayOptions) (*GatewayClient, error) {
	if opts.MaxSize == 0 {
		opts.MaxSize = 8 * 1024 * 1024
	}
	remote := ""
	if opts.Endpoint == nil {
		remote = strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL"))
	}
	var target string
	var protocols []string
	switch {
	case opts.Endpoint != nil:
		profileHome, err := GatewayProfileHome(home.Dir(ctx))
		if err != nil {
			return nil, err
		}
		ticket, err := sessionTicket(profileHome, opts.Endpoint, "", "", opts.Subject, false)
		if err != nil {
			return nil, err
		}
		target, protocols = GatewayWSTarget(opts.Endpoint, ticket)
	case remote != "":
		parsed, err := url.Parse(remote)
		if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") || parsed.Hostname() == "" {
			return nil, &GatewayClientError{Msg: "Invalid explicit gateway WebSocket URL; no local fallback"}
		}
		target = remote
	default:
		profileHome, err := GatewayProfileHome(home.Dir(ctx))
		if err != nil {
			return nil, err
		}
		result, err := ensureGatewayRuntime(ctx, profileHome, true)
		if err != nil {
			return nil, err
		}
		if !Attachable(result) || (refusesRestartWait(ctx) && result.State != "ready") {
			if result.ReasonCode == "runtime_exited" && result.Detail != "" {
				return nil, &GatewayUnavailableError{Msg: "Gateway could not start: " + result.Detail, State: result.State, ReasonCode: result.ReasonCode}
			}
			detail := ""
			if result.Detail != "" {
				detail = " (" + result.Detail + ")"
			}
			reason := result.ReasonCode
			if reason == "" {
				reason = "not_ready"
			}
			return nil, &GatewayUnavailableError{Msg: fmt.Sprintf("Gateway %s: %s%s", result.State, reason, detail), State: result.State, ReasonCode: result.ReasonCode}
		}
		ticket, err := sessionTicket(profileHome, result.Endpoint, "", "", "", refusesRestartWait(ctx))
		var discovery *DiscoveryError
		if errors.As(err, &discovery) {
			detail := discovery.BoundedDetail(500)
			msg := "Gateway inaccessible: " + discovery.Reason
			if detail != "" {
				msg += " (" + detail + ")"
			}
			return nil, &GatewayUnavailableError{Msg: msg, State: "inaccessible", ReasonCode: discovery.Reason}
		}
		if err != nil {
			return nil, err
		}
		target, protocols = GatewayWSTarget(result.Endpoint, ticket)
	}
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 10 * time.Second, Subprotocols: protocols}
	conn, resp, err := dialer.DialContext(ctx, target, http.Header{})
	if err != nil {
		if resp != nil && errors.Is(err, websocket.ErrBadHandshake) {
			return nil, &InvalidStatusError{StatusCode: resp.StatusCode}
		}
		var netErr net.Error
		var opErr *net.OpError
		if errors.As(err, &netErr) || errors.As(err, &opErr) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return nil, &GatewayClientError{Msg: "Gateway connection failed; no local fallback"}
		}
		return nil, err
	}
	conn.SetReadLimit(opts.MaxSize)
	sock := &wsSocket{conn: conn}
	if protocols != nil && sock.Subprotocol() != CanonicalGatewayProtocol {
		sock.Close()
		return nil, &GatewayClientError{Msg: "Gateway protocol mismatch; update/restart required"}
	}
	return NewGatewayClient(sock).Start(), nil
}
