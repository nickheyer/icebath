package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Bounds of one connection, the dispatch queue, the side lane of expensive reads and the wait for a profile start
const (
	wsLogPayloadPreview   = 240
	wsDispatchQueueMax    = 256
	wsConcurrentReadLimit = 2
	wsWaitingReadLimit    = 8
)

// Wait of a frame for the profile start it names, under the Desktop request deadline
var wsProfileStartWait = 20 * time.Second

// Seconds between two marker writes of the dashboard client liveness throttle
var dashboardClientTouchMinIntervalS = 5.0

// Read only projections that run beside the serial lane, and verbs that answer an open server request
var (
	wsConcurrentReadMethods = setOf("session.context_breakdown")
	wsAnswerMethods         = setOf("clarify.lock")
)

func wsLogger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.ws") }

// Seams of the socket entry, the real functions swapped only where a hermes case installs a fake
var (
	wsServerDispatch            = Dispatch
	wsDispatchRequestFn         = wsDispatchRequest
	wsNewAuthorityConnection    = newWSAuthorityConnection
	wsCloseSessionsForTransport = closeSessionsForTransport
	wsReleaseWakeForTransport   = releaseWakeForTransport
	wsEnsureSkinWatcher         = ensureSkinWatcher
	wsEnsureLeaseWatcher        = ensureLeaseWatcher
	wsStartBackendLiveness      = startBackendLiveness
	wsStartHeartbeatRefresher   = startBackendHeartbeatRefresher
	wsResolveSkin               = resolveSkin
	wsTouchDashboardClient      = gateway.TouchDashboardClientHeartbeat
	wsSubmitDashboardClient     = submitDashboardClientTouch
)

// Throttle and single writer of the dashboard client liveness marker
var dashboardClientTouch = struct {
	sync.Mutex
	touchedAt float64
	pending   map[string]bool
	queue     []string
	running   bool
}{pending: map[string]bool{}}

// Clears the pending mark of a path and stamps its marker
func touchDashboardClientMarker(path string) {
	dashboardClientTouch.Lock()
	delete(dashboardClientTouch.pending, path)
	dashboardClientTouch.Unlock()
	wsTouchDashboardClient(path)
}

// Queues one marker write on the single toucher, starting it when idle
func submitDashboardClientTouch(path string) error {
	dashboardClientTouch.Lock()
	defer dashboardClientTouch.Unlock()
	dashboardClientTouch.queue = append(dashboardClientTouch.queue, path)
	if dashboardClientTouch.running {
		return nil
	}
	dashboardClientTouch.running = true
	go func() {
		for {
			dashboardClientTouch.Lock()
			if len(dashboardClientTouch.queue) == 0 {
				dashboardClientTouch.running = false
				dashboardClientTouch.Unlock()
				return
			}
			next := dashboardClientTouch.queue[0]
			dashboardClientTouch.queue = dashboardClientTouch.queue[1:]
			dashboardClientTouch.Unlock()
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						wsLogger().Debug(fmt.Sprintf("dashboard client heartbeat touch failed: %v", failure))
					}
				}()
				touchDashboardClientMarker(next)
			}()
		}
	}()
	return nil
}

// Refreshes the dashboard client liveness marker at most every five seconds without blocking the caller
func noteDashboardClientActivity(force bool) {
	now := pythread.Monotonic()
	dashboardClientTouch.Lock()
	if !force && now-dashboardClientTouch.touchedAt < dashboardClientTouchMinIntervalS {
		dashboardClientTouch.Unlock()
		return
	}
	dashboardClientTouch.touchedAt = now
	dashboardClientTouch.Unlock()
	path := gateway.DashboardClientHeartbeatPath("")
	dashboardClientTouch.Lock()
	if dashboardClientTouch.pending[path] {
		dashboardClientTouch.Unlock()
		return
	}
	dashboardClientTouch.pending[path] = true
	dashboardClientTouch.Unlock()
	if err := wsSubmitDashboardClient(path); err != nil {
		dashboardClientTouch.Lock()
		delete(dashboardClientTouch.pending, path)
		dashboardClientTouch.Unlock()
		wsLogger().Debug(fmt.Sprintf("dashboard client heartbeat touch failed: %s", err))
	}
}

// Whether a frame answers an open server request, settled at once instead of queued
func answersOpenRequest(req any) bool {
	if m, isDict := plainRequest(req).(map[string]any); isDict {
		if method, _ := m["method"].(string); wsAnswerMethods[method] {
			return true
		}
		return tuigateway.IsResponseFrame(frameObject(m))
	}
	return false
}

// Host and port of a socket peer, the port nil when unknown
type SocketPeer struct {
	Host string
	Port *int
}

// Peer close a socket read reports, the starlette WebSocketDisconnect with its code and reason
type SocketDisconnect struct {
	Code   any
	Reason any
}

func (e *SocketDisconnect) Error() string {
	return fmt.Sprintf("(%s, %s)", pyrepr.Repr(e.Code), pyrepr.Repr(e.Reason))
}

// Accepted WebSocket one session connection runs over, as the dashboard and gateway routes hand it in
type ServedSocket interface {
	tuigateway.WSConn
	Accept(ctx context.Context, subprotocol *string) error
	// Next text frame, a SocketDisconnect once the peer leaves
	ReceiveText(ctx context.Context) (string, error)
	// Peer address, nil when the server reports none
	Client() *SocketPeer
	// Raw stream socket under the connection, nil when the server exposes none
	RawConn() net.Conn
	// Session authority of the route scope, else of the app state, nil when the backend has none
	SessionAuthority() *gateway.SessionAuthority
}

// Peer label of a socket, host and port when known
func wsPeerLabel(ws ServedSocket) string {
	client := ws.Client()
	if client == nil {
		return "unknown"
	}
	host := client.Host
	if host == "" {
		host = "unknown"
	}
	if client.Port != nil {
		return fmt.Sprintf("%s:%d", host, *client.Port)
	}
	return host
}

// Whether a response is the method not found error
func isUnknownMethod(resp any) bool {
	switch frame := resp.(type) {
	case map[string]any:
		errObj, isDict := frame["error"].(map[string]any)
		return isDict && pyrepr.Equal(errObj["code"], -32601)
	case *pyjson.Object:
		if frame == nil {
			return false
		}
		switch errObj := frame.GetOr("error", nil).(type) {
		case *pyjson.Object:
			return errObj != nil && pyrepr.Equal(errObj.GetOr("code", nil), -32601)
		case map[string]any:
			return pyrepr.Equal(errObj["code"], -32601)
		}
	}
	return false
}

// Disables Nagle and enables TCP keepalive on the raw socket, logging when the server exposes none it can tune
func disableNagle(ws ServedSocket) {
	conn, isTCP := ws.RawConn().(*net.TCPConn)
	if !isTCP {
		return
	}
	if err := conn.SetNoDelay(true); err != nil {
		wsLogger().Debug(fmt.Sprintf("ws TCP_NODELAY skip: %s", err))
		return
	}
	keepalive := net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}
	if runtime.GOOS == "darwin" {
		keepalive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: -1, Count: -1}
	}
	if err := conn.SetKeepAliveConfig(keepalive); err != nil {
		wsLogger().Debug(fmt.Sprintf("ws TCP_NODELAY skip: %s", err))
	}
}

// Session authority projection of one socket that requests go through when the backend serves one
type authorityConn interface {
	Dispatch(ctx context.Context, request *pyjson.Object) (*pyjson.Object, error)
	Close(ctx context.Context) error
	Actor() gateway.Principal
	AwaitsProfileStart(ctx context.Context, request map[string]any, held []string) (string, <-chan struct{}, bool, error)
}

// Authority connection of the socket, nil when the backend has no authority and requests take the legacy dispatcher
func wsAuthorityConnection(ws ServedSocket, transport *tuigateway.WSTransport, authIdentity map[string]any, operator bool) (authorityConn, error) {
	authority := ws.SessionAuthority()
	if authority == nil {
		return nil, nil
	}
	if authIdentity == nil {
		authIdentity = map[string]any{}
	}
	return wsNewAuthorityConnection(authority, transport, authIdentity, operator)
}

// Builds the authority projection of one socket, the constructor a hermes case replaces with its own connection
func newWSAuthorityConnection(authority *gateway.SessionAuthority, transport *tuigateway.WSTransport, identity map[string]any, operator bool) (authorityConn, error) {
	conn, err := gateway.NewAuthorityConnection(authority, transport, identity, operator)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Capability names a principal holds
func principalCapabilities(actor gateway.Principal) []string {
	out := make([]string, 0, len(actor.Capabilities))
	for name, held := range actor.Capabilities {
		if held {
			out = append(out, name)
		}
	}
	return out
}

// One request through the authority when attached, else the legacy dispatcher, run as work for a local Desktop
func wsDispatchRequest(ctx context.Context, authority authorityConn, req any, reqMethod string, transport *tuigateway.WSTransport) (any, error) {
	ctx = tools.WithServingLocalDesktop(ctx, transport.DesktopLocal)
	if authority == nil {
		if resp := wsServerDispatch(ctx, req, transport); resp != nil {
			return resp, nil
		}
		return nil, nil
	}
	request, isDict := req.(*pyjson.Object)
	if !isDict || request == nil {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(req))
	}
	reply, err := authority.Dispatch(ctx, request)
	if err != nil {
		return nil, err
	}
	var resp any
	if reply != nil {
		resp = reply
	}
	actor := authority.Actor()
	if isUnknownMethod(resp) && lookupMethod(reqMethod) != nil && legacyFallbackAllowed(principalCapabilities(actor), reqMethod) {
		reqMap, _ := plainRequest(req).(map[string]any)
		legacy := dispatchLegacy(ctx, reqMap, transport, actor)
		if legacy == nil {
			return nil, nil
		}
		resp = legacy
	}
	return resp, nil
}

// Starts the backend heartbeat refresher and the startup orphan sweep once, logging a failure of either
func startBackendLiveness(ctx context.Context) {
	for _, step := range []struct {
		start func(context.Context)
		what  string
	}{
		{wsStartHeartbeatRefresher, "backend heartbeat refresher start"},
		{scheduleStartupOrphanSweep, "startup orphan sweep scheduling"},
	} {
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					wsLogger().Warn(fmt.Sprintf("%s failed: %v", step.what, failure))
				}
			}()
			step.start(ctx)
		}()
	}
}

// Error ending the read loop when a reply could not be written
var errWSSendFailed = errors.New("ws send failed")

// Queue sentinel that stops the dispatcher, distinct from a JSON null frame
type wsStopSentinel struct{}

// One expensive read admitted beside the serial lane
type wsRead struct {
	started   bool
	cancelled bool
	admit     chan struct{}
	cancel    chan struct{}
	done      chan struct{}
}

// State of one socket session shared by the reader, the dispatcher and the side lanes
type wsConnection struct {
	ctx       context.Context
	ws        ServedSocket
	peer      string
	transport *tuigateway.WSTransport
	authority authorityConn

	mu                                                   sync.Mutex
	messages, parseErrors, dispatchCrashes, sendFailures int
	disconnectReason                                     string
	pending                                              chan any
	dispatcherDone                                       chan struct{}
	dispatcherFailure                                    any
	sendFailed                                           chan struct{}
	sendFailedOnce                                       sync.Once
	readAdmission, heldRoom                              chan struct{}
	runningReads                                         int
	waitingReads                                         []*wsRead
	held                                                 map[string][]any
	heldKeys                                             []string
	heldLanes                                            sync.WaitGroup
	laneCancel                                           chan struct{}
	clientGone                                           chan struct{}
	clientGoneOnce                                       sync.Once
	heldWaiting                                          map[string]<-chan struct{}
	heldDropped                                          map[string]bool
	reads                                                map[*wsRead]bool
}

// Marks that the read lane could not send a reply
func (c *wsConnection) setSendFailed() { c.sendFailedOnce.Do(func() { close(c.sendFailed) }) }

// Writes a reply, recording reason and ending the read loop when it could not be sent
func (c *wsConnection) reply(frame any, reason, msg string) error {
	if c.transport.WriteAsync(frameObject(frame)) {
		return nil
	}
	c.mu.Lock()
	c.disconnectReason = reason
	c.sendFailures++
	c.mu.Unlock()
	wsLogger().Warn(msg)
	return errWSSendFailed
}

// JSON RPC error frame with its members in the order the Python literal writes them
func wsError(code int, message string, reqID any) *pyjson.Object {
	return pyjson.ObjectOf("jsonrpc", "2.0", "error", pyjson.ObjectOf("code", code, "message", message), "id", reqID)
}

// Id and method of a request object, nil and empty for anything else
func requestIDAndMethod(req any) (any, string) {
	m, isDict := plainRequest(req).(map[string]any)
	if !isDict {
		return nil, ""
	}
	method, _ := m["method"].(string)
	return m["id"], method
}

// Method member of a request as the log lines print it, None when the frame carries none
func requestMethodText(req any) string {
	if obj, isDict := req.(*pyjson.Object); isDict && obj != nil {
		return pyrepr.Str(obj.GetOr("method", nil))
	}
	return "None"
}

// Fails like a set lookup of a list or dict method, which ends the Python read loop with a TypeError
func requestMethodHashable(req any) error {
	obj, isDict := req.(*pyjson.Object)
	if !isDict || obj == nil {
		return nil
	}
	switch obj.GetOr("method", nil).(type) {
	case []any:
		return &pyjson.TypeError{Msg: "unhashable type: 'list'"}
	case *pyjson.Object:
		return &pyjson.TypeError{Msg: "unhashable type: 'dict'"}
	}
	return nil
}

// Dispatches one request and writes its inline reply, a crash answered with -32603
func (c *wsConnection) dispatchOne(req any) error {
	reqID, reqMethod := requestIDAndMethod(req)
	methodText := requestMethodText(req)
	var resp any
	var crash any
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				crash = failure
			}
		}()
		var err error
		resp, err = wsDispatchRequestFn(c.ctx, c.authority, req, reqMethod, c.transport)
		if err != nil {
			crash = err
		}
	}()
	if crash != nil {
		c.mu.Lock()
		c.dispatchCrashes++
		c.mu.Unlock()
		wsLogger().Error(fmt.Sprintf("ws dispatch crash peer=%s id=%s method=%s: %s", c.peer, pyrepr.Str(reqID), methodText, failureText(crash)))
		return c.reply(wsError(-32603, "internal error", reqID), "send_failed_after_dispatch_crash",
			fmt.Sprintf("ws dispatch-crash reply send failed peer=%s id=%s method=%s", c.peer, pyrepr.Str(reqID), methodText))
	}
	if resp != nil {
		return c.reply(resp, "send_failed_after_response",
			fmt.Sprintf("ws response send failed peer=%s id=%s method=%s", c.peer, pyrepr.Str(reqID), methodText))
	}
	return nil
}

// Takes one place of a semaphore unless the dispatcher ended or a reply failed
func (c *wsConnection) acquire(sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-c.dispatcherDone:
		return errWSSendFailed
	case <-c.sendFailed:
		return errWSSendFailed
	}
}

// Puts a frame on the serial queue unless the dispatcher ended or a reply failed
func (c *wsConnection) put(item any) error {
	select {
	case c.pending <- item:
		return nil
	default:
	}
	select {
	case c.pending <- item:
		return nil
	case <-c.dispatcherDone:
		return errWSSendFailed
	case <-c.sendFailed:
		return errWSSendFailed
	}
}

// Hands the freed side lane slot to the oldest waiting read, called with the connection lock held
func (c *wsConnection) passReadSlotLocked() {
	for len(c.waitingReads) > 0 {
		next := c.waitingReads[0]
		c.waitingReads = c.waitingReads[1:]
		if !next.cancelled {
			close(next.admit)
			return
		}
	}
	c.runningReads--
}

// Admits a read to the side lane in arrival order, at once while a slot is free
func (c *wsConnection) admitRead(read *wsRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[read] = true
	if c.runningReads < wsConcurrentReadLimit {
		c.runningReads++
		close(read.admit)
		return
	}
	c.waitingReads = append(c.waitingReads, read)
}

// Runs one admitted expensive read once a side lane slot reaches it, a cancel before it starts skipping it
func (c *wsConnection) dispatchConcurrentRead(read *wsRead, req any) {
	defer func() {
		<-c.readAdmission
		c.mu.Lock()
		delete(c.reads, read)
		c.mu.Unlock()
		close(read.done)
	}()
	select {
	case <-read.admit:
	case <-read.cancel:
	}
	c.mu.Lock()
	if !channelClosed(read.admit) {
		for i, waiting := range c.waitingReads {
			if waiting == read {
				c.waitingReads = append(c.waitingReads[:i], c.waitingReads[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		return
	}
	if read.cancelled {
		c.passReadSlotLocked()
		c.mu.Unlock()
		return
	}
	read.started = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.passReadSlotLocked()
		c.mu.Unlock()
	}()
	if errors.Is(c.dispatchOne(req), errWSSendFailed) {
		c.setSendFailed()
	}
}

// Forgets one start lane, called with the connection lock held so no frame joins a lane that ended
func (c *wsConnection) forgetLaneLocked(key string) {
	delete(c.heldWaiting, key)
	delete(c.heldDropped, key)
	if _, holding := c.held[key]; !holding {
		return
	}
	delete(c.held, key)
	for i, k := range c.heldKeys {
		if k == key {
			c.heldKeys = append(c.heldKeys[:i], c.heldKeys[i+1:]...)
			break
		}
	}
}

// Dispatches the frames held for one start in arrival order once it ends or the wait runs out
func (c *wsConnection) dispatchAfterStart(key string, started <-chan struct{}) {
	defer c.heldLanes.Done()
	defer func() {
		c.mu.Lock()
		c.forgetLaneLocked(key)
		c.mu.Unlock()
	}()
	timer := time.NewTimer(wsProfileStartWait)
	defer timer.Stop()
	select {
	case <-started:
	case <-c.clientGone:
	case <-timer.C:
	case <-c.laneCancel:
		return
	}
	c.mu.Lock()
	delete(c.heldWaiting, key)
	if c.heldDropped[key] {
		for range c.held[key] {
			<-c.heldRoom
		}
		c.forgetLaneLocked(key)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	for {
		select {
		case <-c.laneCancel:
			return
		default:
		}
		c.mu.Lock()
		frames := c.held[key]
		if len(frames) == 0 {
			c.forgetLaneLocked(key)
			c.mu.Unlock()
			return
		}
		frame := frames[0]
		c.mu.Unlock()
		if errors.Is(c.dispatchOne(frame), errWSSendFailed) {
			c.setSendFailed()
			return
		}
		c.mu.Lock()
		c.held[key] = c.held[key][1:]
		c.mu.Unlock()
		<-c.heldRoom
	}
}

// Start a frame waits for with the held starts as they stand, called with the connection lock held
func (c *wsConnection) awaitsProfileStartLocked(req any) (string, <-chan struct{}, bool, error) {
	reqMap, isDict := plainRequest(req).(map[string]any)
	if !isDict {
		reqMap = nil
	}
	return c.authority.AwaitsProfileStart(c.ctx, reqMap, c.heldKeys)
}

// Holds a frame naming a profile still starting in its own lane, false when the serial lane takes it
func (c *wsConnection) holdForProfileStart(req any) (bool, error) {
	if c.authority == nil {
		return false, nil
	}
	c.mu.Lock()
	_, _, ok, err := c.awaitsProfileStartLocked(req)
	c.mu.Unlock()
	if err != nil || !ok {
		return false, err
	}
	if err := c.acquire(c.heldRoom); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key, started, ok, err := c.awaitsProfileStartLocked(req)
	if err != nil || !ok {
		<-c.heldRoom
		return false, err
	}
	if _, holding := c.held[key]; holding {
		c.held[key] = append(c.held[key], req)
		return true, nil
	}
	if started == nil {
		panic("a start not held yet comes from the registry with its event")
	}
	c.held[key] = []any{req}
	c.heldKeys = append(c.heldKeys, key)
	c.heldWaiting[key] = started
	if channelClosed(c.clientGone) && !channelClosed(started) {
		c.heldDropped[key] = true
	}
	c.heldLanes.Add(1)
	go c.dispatchAfterStart(key, started)
	return true, nil
}

// Drops every lane whose start had not ended when the client left
func (c *wsConnection) clientLeft() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientGoneOnce.Do(func() { close(c.clientGone) })
	for key, started := range c.heldWaiting {
		if !channelClosed(started) {
			c.heldDropped[key] = true
		}
	}
}

// Next queued frame, or the stop sentinel once the read lane failed to send a reply
func (c *wsConnection) nextRequest() any {
	select {
	case req := <-c.pending:
		select {
		case <-c.sendFailed:
			return wsStopSentinel{}
		default:
			return req
		}
	case <-c.sendFailed:
		return wsStopSentinel{}
	case <-c.ctx.Done():
		return wsStopSentinel{}
	}
}

// Dispatches controls in arrival order with bounded expensive reads running beside them
func (c *wsConnection) dispatchLoop() error {
	for {
		req := c.nextRequest()
		if _, stop := req.(wsStopSentinel); stop {
			return nil
		}
		_, reqMethod := requestIDAndMethod(req)
		if wsConcurrentReadMethods[reqMethod] {
			read := &wsRead{admit: make(chan struct{}), cancel: make(chan struct{}), done: make(chan struct{})}
			c.admitRead(read)
			go c.dispatchConcurrentRead(read, req)
			continue
		}
		held, err := c.holdForProfileStart(req)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		if err := c.dispatchOne(req); err != nil {
			return err
		}
	}
}

// Settles an answer at once, else admits an expensive read or queues the frame for the serial lane
func (c *wsConnection) queueRequest(req any) error {
	if err := requestMethodHashable(req); err != nil {
		return err
	}
	if answersOpenRequest(req) {
		return c.dispatchOne(req)
	}
	if _, reqMethod := requestIDAndMethod(req); wsConcurrentReadMethods[reqMethod] {
		if err := c.acquire(c.readAdmission); err != nil {
			return err
		}
	}
	return c.put(req)
}

// Drops reads that never started and waits for started ones unless the connection was cancelled
func (c *wsConnection) endReads() {
	shuttingDown := c.ctx.Err() != nil
	c.mu.Lock()
	awaited := make([]*wsRead, 0, len(c.reads))
	for read := range c.reads {
		if shuttingDown || !read.started {
			read.cancelled = true
			close(read.cancel)
		}
		if !(read.started && shuttingDown) {
			awaited = append(awaited, read)
		}
	}
	c.mu.Unlock()
	for _, read := range awaited {
		<-read.done
	}
}

// Finishes the lanes whose start ended before the disconnect, cancelling them all when the connection was cancelled
func (c *wsConnection) endHeld() {
	if c.ctx.Err() != nil {
		close(c.laneCancel)
		return
	}
	c.heldLanes.Wait()
}

// Next text frame of the socket unless the dispatcher ended or a reply failed meanwhile
func (c *wsConnection) receive() (string, error) {
	type received struct {
		text string
		err  error
	}
	got := make(chan received, 1)
	go func() {
		text, err := c.ws.ReceiveText(c.ctx)
		got <- received{text, err}
	}()
	select {
	case r := <-got:
		return r.text, r.err
	case <-c.dispatcherDone:
	case <-c.sendFailed:
	}
	select {
	case r := <-got:
		return r.text, r.err
	default:
		return "", errWSSendFailed
	}
}

// Runs one WebSocket session wire compatible with the stdio entry, the identity the upgrade verified owning its sessions
func HandleWS(ctx context.Context, ws ServedSocket, authIdentity map[string]any, subprotocol *string, operator bool) error {
	c := &wsConnection{ctx: ctx, ws: ws, peer: wsPeerLabel(ws), disconnectReason: "not_connected",
		pending:       make(chan any, wsDispatchQueueMax),
		sendFailed:    make(chan struct{}),
		readAdmission: make(chan struct{}, wsConcurrentReadLimit+wsWaitingReadLimit),
		heldRoom:      make(chan struct{}, wsDispatchQueueMax),
		held:          map[string][]any{}, laneCancel: make(chan struct{}), clientGone: make(chan struct{}),
		heldWaiting: map[string]<-chan struct{}{}, heldDropped: map[string]bool{}, reads: map[*wsRead]bool{}}
	failure := c.serve(subprotocol, authIdentity, operator)
	if err := c.teardown(); err != nil {
		return err
	}
	if errors.Is(failure, errWSSendFailed) {
		return nil
	}
	return failure
}

// Accepts, announces readiness and reads frames until the peer leaves or a reply fails
func (c *wsConnection) serve(subprotocol *string, authIdentity map[string]any, operator bool) (failure error) {
	defer func() {
		if crash := recover(); crash != nil {
			if err, isError := crash.(error); isError {
				failure = err
			} else {
				failure = errors.New(failureText(crash))
			}
		}
	}()
	if subprotocol != nil && *subprotocol == "" {
		subprotocol = nil
	}
	if err := c.ws.Accept(c.ctx, subprotocol); err != nil {
		return err
	}
	c.disconnectReason = "connected"
	noteDashboardClientActivity(true)
	disableNagle(c.ws)
	wsLogger().Info(fmt.Sprintf("ws accepted peer=%s", c.peer))
	c.transport = tuigateway.NewWSTransport(c.ws, c.peer, authIdentity)
	authority, err := wsAuthorityConnection(c.ws, c.transport, authIdentity, operator)
	if err != nil {
		return err
	}
	c.authority = authority
	skinPayload := wsResolveSkin(c.ctx)
	readyPayload := pyjson.ObjectOf("skin", pyjsonValue(skinPayload), "change_events", true, "heartbeat", true,
		"replay_epoch", tuigateway.ReplayEpoch())
	if authority != nil {
		readyPayload.Set("session_authority", true)
	}
	readyOK := c.transport.WriteAsync(pyjson.ObjectOf("jsonrpc", "2.0", "method", "event",
		"params", pyjson.ObjectOf("type", "gateway.ready", "payload", readyPayload)))
	if readyOK {
		wsEnsureSkinWatcher(c.ctx)
		wsEnsureLeaseWatcher()
		if authority != nil {
			c.transport.SkinHome = skinHomeFor(c.ctx, authority.Actor().ProfileID)
		}
		tuigateway.RegisterLiveTransport(c.transport)
	}
	wsStartBackendLiveness(c.ctx)
	if !readyOK {
		c.mu.Lock()
		c.disconnectReason = "ready_send_failed"
		c.sendFailures++
		c.mu.Unlock()
		wsLogger().Error(fmt.Sprintf("ws ready frame send failed peer=%s", c.peer))
		return nil
	}
	c.dispatcherDone = make(chan struct{})
	go func() {
		defer close(c.dispatcherDone)
		defer func() {
			if failure := recover(); failure != nil {
				c.dispatcherFailure = failure
			}
		}()
		if err := c.dispatchLoop(); err != nil {
			c.dispatcherFailure = err
		}
	}()
	for {
		raw, err := c.receive()
		if errors.Is(err, errWSSendFailed) {
			return err
		}
		if err != nil && c.ctx.Err() != nil {
			return c.ctx.Err()
		}
		var disconnect *SocketDisconnect
		if errors.As(err, &disconnect) {
			c.mu.Lock()
			c.disconnectReason = fmt.Sprintf("client_disconnect(code=%s,reason=%s)", pyrepr.Str(disconnect.Code), pyrepr.Str(disconnect.Reason))
			c.mu.Unlock()
			return nil
		}
		if err != nil {
			c.mu.Lock()
			c.disconnectReason = "receive_failed"
			c.mu.Unlock()
			wsLogger().Error(fmt.Sprintf("ws receive failed peer=%s: %s", c.peer, err))
			return nil
		}
		noteDashboardClientActivity(false)
		line := pystr.Strip(raw)
		if line == "" {
			continue
		}
		c.mu.Lock()
		c.messages++
		index := c.messages
		c.mu.Unlock()
		req, err := pyjson.Loads(line)
		if err != nil {
			c.mu.Lock()
			c.parseErrors++
			c.mu.Unlock()
			preview := []rune(line)
			if len(preview) > wsLogPayloadPreview {
				preview = preview[:wsLogPayloadPreview]
			}
			wsLogger().Warn(fmt.Sprintf("ws parse error peer=%s index=%d error=%s payload=%s", c.peer, index, err, pyrepr.Repr(string(preview))))
			if err := c.reply(wsError(-32700, "parse error", nil), "send_failed_after_parse_error",
				fmt.Sprintf("ws parse-error reply send failed peer=%s", c.peer)); err != nil {
				return err
			}
			continue
		}
		if obj, isDict := req.(*pyjson.Object); isDict && obj.GetOr("method", nil) == "gateway.ping" {
			reqID := obj.GetOr("id", nil)
			if err := c.reply(pyjson.ObjectOf("jsonrpc", "2.0", "result", pyjson.ObjectOf("ok", true), "id", reqID), "send_failed_after_heartbeat",
				fmt.Sprintf("ws heartbeat reply send failed peer=%s id=%s", c.peer, pyrepr.Str(reqID))); err != nil {
				return err
			}
			continue
		}
		if err := c.queueRequest(req); err != nil {
			return err
		}
	}
}

// Drains the dispatcher and side lanes, parks or reaps the sessions of this transport and closes the socket
func (c *wsConnection) teardown() error {
	c.clientLeft()
	if c.dispatcherDone != nil {
		if c.ctx.Err() == nil {
			_ = c.put(wsStopSentinel{})
			<-c.dispatcherDone
			if failure := c.dispatcherFailure; failure != nil && failure != errWSSendFailed {
				wsLogger().Error(fmt.Sprintf("ws dispatcher failed peer=%s: %s", c.peer, failureText(failure)))
			}
		}
	}
	c.endReads()
	c.endHeld()
	ctx := context.WithoutCancel(c.ctx)
	if c.authority != nil {
		if err := c.authority.Close(ctx); err != nil {
			return err
		}
	}
	reaped, detached := 0, 0
	if c.transport != nil {
		tuigateway.UnregisterLiveTransport(c.transport)
		c.transport.Close()
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					wsLogger().Error(fmt.Sprintf("ws wake-word teardown failed peer=%s: %s", c.peer, failureText(failure)))
				}
			}()
			wsReleaseWakeForTransport(c.transport)
		}()
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					wsLogger().Error(fmt.Sprintf("ws transport teardown failed peer=%s: %s", c.peer, failureText(failure)))
				}
			}()
			reaped, detached = wsCloseSessionsForTransport(ctx, c.transport, "ws_disconnect")
		}()
	}
	if err := c.ws.Close(1000); err != nil {
		wsLogger().Debug(fmt.Sprintf("ws close failed peer=%s error=%s", c.peer, err))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wsLogger().Info(fmt.Sprintf("ws closed peer=%s reason=%s messages=%d parse_errors=%d "+
		"dispatch_crashes=%d send_failures=%d reaped_sessions=%d detached_sessions=%d",
		c.peer, c.disconnectReason, c.messages, c.parseErrors, c.dispatchCrashes, c.sendFailures, reaped, detached))
	return nil
}
