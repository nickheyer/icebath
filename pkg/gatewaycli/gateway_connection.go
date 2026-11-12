package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
)

var (
	// Resume refusals meaning this viewer may not open the session, so the caller keeps the one it is on
	ResumeRefusals = map[string]bool{"not_found": true, "permission_denied": true}
	// Resume refusals meaning the session is gone for this viewer
	ResumeGone = map[string]bool{"not_found": true, "permission_denied": true, "profile_mismatch": true}
	// Keyed resume refusals a plain resume answers
	keyNotTaken = map[string]bool{"admission_conflict": true, "permission_denied": true}
	// Side task answers routed to their waiter by task id
	sideEvents = map[string]bool{"background.complete": true, "btw.complete": true}
)

// Dial budget of one connection start
const DefaultStartTimeout = 90 * time.Second

// Raised when the caller stopped waiting for an answer whose request was sent
type CallStopped struct{ Method string }

func (e *CallStopped) Error() string { return e.Method }

// Lets a stopped call match a GatewayClientError target like the Python subclass
func (e *CallStopped) As(target any) bool {
	if t, ok := target.(**gateway.GatewayClientError); ok {
		*t = &gateway.GatewayClientError{Msg: e.Method}
		return true
	}
	return false
}

// Call on the connection loop goroutine, which would wait on itself
type LoopThreadError struct{ Msg string }

func (e *LoopThreadError) Error() string      { return e.Msg }
func (e *LoopThreadError) PyTypeName() string { return "RuntimeError" }

// Transport error tagged with the connection epoch it ended
type ConnectionLoss struct {
	Err             error
	ConnectionEpoch int
}

func (e *ConnectionLoss) Error() string { return e.Err.Error() }
func (e *ConnectionLoss) Unwrap() error { return e.Err }

// Whether err stands for the Python GatewayClientError or one of its subclasses
func isClientError(err error) bool {
	var ce *gateway.GatewayClientError
	return errors.As(err, &ce)
}

// Nil when a refused keyed resume is retried plain, else the error the resume raises
func resumePlain(exc *gateway.GatewayRPCError, raised error, sessionID string, typed bool) error {
	if typed && exc.Error() == "admission_conflict" {
		return &gateway.GatewayClientError{Msg: gateway.ResumeKeyMismatch}
	}
	if typed || !keyNotTaken[exc.Error()] {
		return raised
	}
	slog.Debug(fmt.Sprintf("session.resume %s: launch key not taken (%s); resuming without it", sessionID, exc.Error()))
	return nil
}

// True for a transport error from a connection older than epoch, so no outcome of work submitted on epoch
func LostBefore(frame any, epoch int) bool {
	loss, ok := frame.(*ConnectionLoss)
	return ok && epoch != 0 && loss.ConnectionEpoch < epoch
}

// What the gateway last told this viewer about one session, locked by its embedded mutex
type SessionState struct {
	sync.Mutex
	SessionID           string
	Info                map[string]any
	Title               string
	PlanState           map[string]any
	Revision            any
	SubscriptionID      any
	ExecutionGeneration any
	Messages            []any
	Usage               map[string]any
	UsageListeners      []*SessionListener
	RequestListeners    []*SessionListener
	AdmittedTurn        map[string]any
	Pending             []any
	QueuedInputs        map[string]any
	Adopted             map[string]any
	ArmedAdmission      string
	PendingSeq          int
	QueuedAt            map[string]int
	RecoveredSeq        int
	Missed              map[string]bool
	Gaps                int
	AttachedGaps        int
	ServiceTierSent     *string
}

// Callback a session state hands one usage dict or one withdrawn request, registered and removed by its pointer
type SessionListener func(ctx context.Context, payload map[string]any)

// Listeners without the given one, every registration of it dropped
func WithoutListener(listeners []*SessionListener, listener *SessionListener) []*SessionListener {
	kept := make([]*SessionListener, 0, len(listeners))
	for _, registered := range listeners {
		if registered != listener {
			kept = append(kept, registered)
		}
	}
	return kept
}

func NewSessionState(sessionID string) *SessionState {
	return &SessionState{SessionID: sessionID, Info: map[string]any{}, Usage: map[string]any{}, QueuedInputs: map[string]any{},
		Adopted: map[string]any{}, QueuedAt: map[string]int{}, Missed: map[string]bool{}}
}

// Plain map of a dict value, ok false for anything else
func plainDict(v any) (map[string]any, bool) {
	obj, ok := pyjson.AsObject(v)
	if !ok {
		return nil, false
	}
	plain, _ := pyjson.Plain(obj).(map[string]any)
	return plain, true
}

// Plain list of a list value, ok false for anything else
func plainList(v any) ([]any, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	plain, _ := pyjson.Plain(list).([]any)
	return plain, true
}

// Replaces the cached view with a resume or create snapshot, keeping fields the snapshot does not carry
func (s *SessionState) AdoptSnapshot(snapshot any) {
	snap, _ := pyjson.AsObject(snapshot)
	if snap == nil {
		snap = pyjson.NewObject()
	}
	s.Lock()
	defer s.Unlock()
	info, _ := plainDict(snap.Vals["info"])
	if info == nil {
		info = map[string]any{}
	}
	s.Info = info
	if v, ok := snap.Get("revision"); ok {
		s.Revision = v
	}
	if v, ok := snap.Get("subscription_id"); ok {
		s.SubscriptionID = v
	}
	if v, ok := snap.Get("execution_generation"); ok {
		s.ExecutionGeneration = v
	}
	messages, _ := plainList(snap.Vals["messages"])
	s.Messages = append([]any{}, messages...)
	if plan, ok := snap.Get("todo_state"); ok {
		s.PlanState, _ = plainDict(plan)
	}
	if pending, ok := plainList(snap.Vals["pending"]); ok {
		s.Pending = pending
		s.PendingSeq++
	}
	if title, ok := s.Info["title"].(string); ok && title != "" {
		s.Title = title
	}
}

// Whether the session runs model, and provider when both sides name one
func (s *SessionState) RunsRoute(model, provider string) bool {
	s.Lock()
	defer s.Unlock()
	current, _ := s.Info["model"].(string)
	currentProvider, _ := s.Info["provider"].(string)
	return current != "" && model == current && !(provider != "" && currentProvider != "" && provider != currentProvider)
}

// Folds one session event frame into the cached view and calls the listeners it concerns
func (s *SessionState) ApplyEvent(ctx context.Context, kind any, payload any) {
	name, _ := kind.(string)
	dict, isDict := plainDict(payload)
	switch {
	case (name == "session.info" || name == "session.updated") && isDict:
		s.Lock()
		info := make(map[string]any, len(s.Info)+len(dict))
		for k, v := range s.Info {
			info[k] = v
		}
		for k, v := range dict {
			if k != "session_id" {
				info[k] = v
			}
		}
		s.Info = info
		if pending, ok := dict["pending"].([]any); ok {
			s.Pending = append([]any{}, pending...)
			s.PendingSeq++
		}
		if title, ok := dict["title"].(string); ok && title != "" {
			s.Title = title
		}
		s.Unlock()
	case name == "todo.updated" && isDict:
		items, _ := dict["todos"].([]any)
		s.Lock()
		s.PlanState = map[string]any{"todos": append([]any{}, items...), "revision": dict["revision"]}
		s.Unlock()
	case name == "session.usage" && isDict:
		nested, nestedDict := dict["usage"].(map[string]any)
		flat := isPyInt(dict["api_calls"]) || !nestedDict
		if flat {
			s.ApplyUsage(ctx, dict)
		} else {
			s.ApplyUsage(ctx, nested)
		}
	case name == "request.cancel" && isDict:
		s.Lock()
		listeners := append([]*SessionListener{}, s.RequestListeners...)
		s.Unlock()
		for _, listener := range listeners {
			(*listener)(ctx, dict)
		}
	case name == "session.replay_gap":
		s.Lock()
		s.Gaps++
		s.Unlock()
	}
}

// Whether a decoded value is a Python int, booleans included
func isPyInt(v any) bool {
	switch v.(type) {
	case int, int64, bool, *big.Int:
		return true
	}
	return false
}

// Stores one session cumulative usage dict and hands it to every usage listener
func (s *SessionState) ApplyUsage(ctx context.Context, usage any) {
	dict, ok := plainDict(usage)
	if !ok {
		return
	}
	s.Lock()
	s.Usage = dict
	listeners := append([]*SessionListener{}, s.UsageListeners...)
	s.Unlock()
	for _, listener := range listeners {
		(*listener)(ctx, dict)
	}
}

// Client surface one connection drives, satisfied by the gateway client
type ConnectionClient interface {
	RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error)
	EventQueue() *gateway.EventQueue
	SendFrame(text string) error
	Close() error
}

// Dials one gateway socket, the connect context manager of the Python connection
type ConnectFunc func(ctx context.Context) (ConnectionClient, error)

// One request through a connection or a store, the call argument of a resume
type CallFunc func(ctx context.Context, method string, params map[string]any) (any, error)

// Answer budget and give up poll of one call, a zero Timeout the default budget and a negative one unbounded
type CallOptions struct {
	Timeout time.Duration
	Abandon func() bool
}

type loopKey struct{}

type liveSocket struct {
	client ConnectionClient
	epoch  int
}

// One gateway connection per classic CLI process, its loop goroutine feeding Events and the session caches
type GatewayConnection struct {
	connect           ConnectFunc
	Events            *gateway.EventQueue
	mu                sync.Mutex
	states            map[string]*SessionState
	client            ConnectionClient
	live              *liveSocket
	looping           bool
	thread            *memory.ContextThread
	ready             pythread.Event
	err               error
	stop              chan struct{}
	stopOnce          *sync.Once
	startLock         sync.Mutex
	closed            bool
	detached          bool
	epoch             int
	side              map[string]*gateway.EventQueue
	sideLock          sync.Mutex
	commands          map[*pythread.Event]bool
	launchAPIKey      string
	launchAPIKeyTyped bool
}

var (
	sharedConnMu sync.Mutex
	sharedConn   *GatewayConnection
)

// Connection dialing through connect, the home owner when connect is nil
func NewGatewayConnection(connect ConnectFunc) *GatewayConnection {
	return &GatewayConnection{connect: connect, Events: gateway.NewEventQueue(0), states: map[string]*SessionState{},
		side: map[string]*gateway.EventQueue{}, commands: map[*pythread.Event]bool{}}
}

// Process wide connection, replaced once the previous one is final
func SharedGatewayConnection() *GatewayConnection {
	sharedConnMu.Lock()
	defer sharedConnMu.Unlock()
	if sharedConn == nil || sharedConn.Final() {
		sharedConn = NewGatewayConnection(nil)
	}
	return sharedConn
}

// Dials this home's owner as a connection client
func connectGateway(ctx context.Context) (ConnectionClient, error) {
	client, err := gateway.ConnectGateway(ctx)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Records the launch only key every resume supplies again and whether the user typed it
func (c *GatewayConnection) SetLaunchKey(key string, typed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launchAPIKey, c.launchAPIKeyTyped = key, typed
}

// Launch only key of this CLI and whether it is the typed flag rather than a keyed startup alias
func (c *GatewayConnection) LaunchKey() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.launchAPIKey, c.launchAPIKeyTyped
}

// True when no call dials again, the viewer closed it or its first connect failed
func (c *GatewayConnection) Final() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.detached || (c.err != nil && c.epoch == 0)
}

// True after a close and while the gateway is hung up
func (c *GatewayConnection) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Count of sockets this connection has opened
func (c *GatewayConnection) Epoch() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// Client of the open socket, nil while hung up
func (c *GatewayConnection) Client() ConnectionClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client
}

// Cached view of a session, created on first use
func (c *GatewayConnection) State(sessionID string) *SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[sessionID]
	if state == nil {
		state = NewSessionState(sessionID)
		c.states[sessionID] = state
	}
	return state
}

// Cached view of a session this viewer has used, nil for any other id
func (c *GatewayConnection) LookupState(sessionID string) *SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[sessionID]
}

// Whether ctx belongs to this connection's loop goroutine
func (c *GatewayConnection) onLoop(ctx context.Context) bool {
	owner, _ := ctx.Value(loopKey{}).(*GatewayConnection)
	return owner == c
}

func (c *GatewayConnection) loadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Dials when no socket is open and waits for the dial in progress, redialing after a gateway hang up
func (c *GatewayConnection) Start(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	remaining := func() time.Duration { return max(time.Until(deadline), 0) }
	timedOut := &gateway.GatewayClientError{Msg: "Gateway connection timed out; no local fallback"}
	for {
		c.startLock.Lock()
		if c.Final() {
			c.startLock.Unlock()
			return c.loadErr()
		}
		c.mu.Lock()
		thread, closed := c.thread, c.closed
		c.mu.Unlock()
		if thread != nil && closed && thread.Alive() && !c.onLoop(ctx) {
			thread.Join(remaining())
		}
		if thread == nil || !thread.Alive() {
			c.mu.Lock()
			if thread != nil {
				c.err = nil
				c.ready.Clear()
				c.closed = false
			}
			thread = memory.SpawnContextThread(ctx, "icebath-gateway-loop", c.run)
			c.thread = thread
			c.mu.Unlock()
			thread.Start()
		}
		c.startLock.Unlock()
		if !c.ready.Wait(remaining().Seconds()) {
			return timedOut
		}
		if err := c.loadErr(); err != nil {
			return err
		}
		if c.Client() != nil || c.Final() {
			return nil
		}
		thread.Join(remaining())
		if !time.Now().Before(deadline) {
			return timedOut
		}
	}
}

// Loop goroutine body, every failure reported to the waiting caller through the ready flag
func (c *GatewayConnection) run(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.mu.Lock()
			if c.err == nil {
				c.err = &gateway.GatewayClientError{Msg: "Gateway connection failed; no local fallback"}
			}
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.ready.Set()
	}()
	c.loop(ctx)
}

// One socket life, dial then resubscribe then pump frames until the socket ends or the viewer closes
func (c *GatewayConnection) loop(ctx context.Context) {
	stop := make(chan struct{})
	c.mu.Lock()
	c.looping = true
	c.stop = stop
	c.stopOnce = &sync.Once{}
	connect := c.connect
	c.mu.Unlock()
	if connect == nil {
		connect = connectGateway
	}
	loopCtx, cancel := context.WithCancel(context.WithValue(ctx, loopKey{}, c))
	defer cancel()
	defer func() {
		c.mu.Lock()
		c.client = nil
		c.live = nil
		c.mu.Unlock()
	}()
	failed := func(err error) {
		if !c.ready.IsSet() {
			if !isClientError(err) {
				err = &gateway.GatewayClientError{Msg: fmt.Sprintf("Gateway connection failed; no local fallback (%s)", pyrepr.ExceptionName(err))}
			}
			c.mu.Lock()
			c.err = err
			c.mu.Unlock()
			return
		}
		c.putLoss(&gateway.GatewayClientError{Msg: gateway.GatewayDisconnected})
	}
	client, err := connect(loopCtx)
	if err != nil {
		failed(err)
		return
	}
	defer client.Close()
	if c.Epoch() != 0 {
		if err := c.resubscribe(loopCtx, client); err != nil {
			failed(err)
			return
		}
	}
	c.mu.Lock()
	c.epoch++
	c.client = client
	c.live = &liveSocket{client: client, epoch: c.epoch}
	c.mu.Unlock()
	c.ready.Set()
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		c.pump(loopCtx, client)
	}()
	select {
	case <-pumped:
	case <-stop:
	}
	cancel()
	<-pumped
}

// Queues a transport error tagged with the epoch of the connection it ended
func (c *GatewayConnection) putLoss(err error) {
	c.Events.PutNowait(&ConnectionLoss{Err: err, ConnectionEpoch: c.Epoch()})
}

// Resumes every session this viewer had open on a new socket, dropping the ones gone meanwhile
func (c *GatewayConnection) resubscribe(ctx context.Context, client ConnectionClient) error {
	c.mu.Lock()
	states := make([]*SessionState, 0, len(c.states))
	for _, state := range c.states {
		states = append(states, state)
	}
	c.mu.Unlock()
	for _, state := range states {
		state.Lock()
		subscribed := state.SubscriptionID != nil
		state.Unlock()
		if !subscribed {
			continue
		}
		snapshot, err := c.Resume(ctx, func(ctx context.Context, method string, params map[string]any) (any, error) {
			return client.RPCTimeout(ctx, method, params, 0)
		}, state.SessionID, false)
		if err != nil {
			if !isClientError(err) || !ResumeGone[err.Error()] {
				return err
			}
			slog.Debug(fmt.Sprintf("session.resume %s after reconnect: %s", state.SessionID, err.Error()))
			state.Lock()
			state.SubscriptionID = nil
			state.Unlock()
			continue
		}
		state.AdoptSnapshot(snapshot)
	}
	return nil
}

// Resumes a session through call with the launch key, a refused keyed resume retried plain when allowed
func (c *GatewayConnection) Resume(ctx context.Context, call CallFunc, sessionID string, typed bool) (any, error) {
	if key, _ := c.LaunchKey(); key != "" {
		snapshot, err := call(ctx, "session.resume", map[string]any{"session_id": sessionID, "api_key": key})
		if err == nil {
			return snapshot, nil
		}
		var refusal *gateway.GatewayRPCError
		if !errors.As(err, &refusal) {
			return nil, err
		}
		if raised := resumePlain(refusal, err, sessionID, typed); raised != nil {
			return nil, raised
		}
	}
	return call(ctx, "session.resume", map[string]any{"session_id": sessionID})
}

// Moves frames from the socket to Events, updating session caches and routing side answers on the way
func (c *GatewayConnection) pump(ctx context.Context, client ConnectionClient) {
	for {
		frame, err := client.EventQueue().Get(ctx)
		if err != nil {
			return
		}
		if failure, ok := frame.(error); ok {
			c.putLoss(failure)
			return
		}
		var params *pyjson.Object
		if dict, ok := pyjson.AsObject(frame); ok {
			params, _ = pyjson.AsObject(dict.Vals["params"])
		}
		if params != nil && c.takeSide(params) {
			continue
		}
		if params != nil {
			if sid, ok := params.Vals["session_id"].(string); ok {
				if state := c.LookupState(sid); state != nil {
					c.applyEvent(ctx, state, params)
				}
			}
		}
		c.Events.PutNowait(frame)
	}
}

// Applies one frame to a session cache, a failing listener never stopping the pump
func (c *GatewayConnection) applyEvent(ctx context.Context, state *SessionState, params *pyjson.Object) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Debug("session state update failed", "error", recovered)
		}
	}()
	state.ApplyEvent(ctx, params.Vals["type"], params.Vals["payload"])
}

// Hands a side task answer to its waiter, false for every other frame
func (c *GatewayConnection) takeSide(params *pyjson.Object) bool {
	kind, _ := params.Vals["type"].(string)
	payload, isDict := plainDict(params.Vals["payload"])
	if !sideEvents[kind] || !isDict || !pyrepr.Truthy(payload["task_id"]) {
		return false
	}
	taskID, ok := payload["task_id"].(string)
	if !ok {
		taskID = pyrepr.Str(payload["task_id"])
	}
	c.SideWaiter(taskID).PutNowait(payload)
	return true
}

// Answer queue of a side task, created by whichever of the waiter or the frame comes first
func (c *GatewayConnection) SideWaiter(taskID string) *gateway.EventQueue {
	c.sideLock.Lock()
	defer c.sideLock.Unlock()
	queue := c.side[taskID]
	if queue == nil {
		queue = gateway.NewEventQueue(0)
		c.side[taskID] = queue
	}
	return queue
}

// Forgets the answer queue of a side task
func (c *GatewayConnection) DropSideWaiter(taskID string) {
	c.sideLock.Lock()
	defer c.sideLock.Unlock()
	delete(c.side, taskID)
}

// Ends the viewer, stopping the loop and every command socket so no later call dials again
func (c *GatewayConnection) Close(ctx context.Context) {
	c.mu.Lock()
	c.detached = true
	c.mu.Unlock()
	c.sideLock.Lock()
	for wake := range c.commands {
		wake.Set()
	}
	c.sideLock.Unlock()
	c.mu.Lock()
	looping, stop, once, closed, thread := c.looping, c.stop, c.stopOnce, c.closed, c.thread
	c.mu.Unlock()
	if looping && stop != nil && !closed {
		once.Do(func() { close(stop) })
	}
	if thread != nil && !c.onLoop(ctx) {
		thread.Join(5 * time.Second)
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

// Answers one server request with a response frame carrying its id, false when the frame was not sent
func (c *GatewayConnection) Reply(requestID any, result any) bool {
	c.mu.Lock()
	client, looping := c.client, c.looping
	c.mu.Unlock()
	if client == nil || !looping {
		return false
	}
	frame, err := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", requestID, "result", result))
	if err == nil {
		sent := make(chan error, 1)
		go func() { sent <- client.SendFrame(frame) }()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case err = <-sent:
		case <-timer.C:
			err = os.ErrDeadlineExceeded
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("server request reply failed: %s", err.Error()))
		return false
	}
	return true
}

// One request and its answer on the live socket under the default budget
func (c *GatewayConnection) Call(ctx context.Context, method string, params map[string]any) (any, error) {
	result, _, err := c.CallTagged(ctx, method, params, CallOptions{})
	return result, err
}

// One request and its answer under an explicit budget and give up poll
func (c *GatewayConnection) CallWith(ctx context.Context, method string, params map[string]any, o CallOptions) (any, error) {
	result, _, err := c.CallTagged(ctx, method, params, o)
	return result, err
}

// Resumes a session after a replay gap, its snapshot naming the subscription to detach
func (c *GatewayConnection) Reattach(ctx context.Context, sessionID string, abandon func() bool) (any, error) {
	state := c.State(sessionID)
	state.Lock()
	gaps := state.Gaps
	state.Unlock()
	snapshot, err := c.CallWith(ctx, "session.resume", map[string]any{"session_id": sessionID}, CallOptions{Abandon: abandon})
	if err != nil {
		return nil, err
	}
	state.AdoptSnapshot(snapshot)
	state.Lock()
	state.AttachedGaps = gaps
	state.Unlock()
	return snapshot, nil
}

type rpcOutcome struct {
	result any
	err    error
}

type rpcFuture struct {
	done   chan rpcOutcome
	cancel context.CancelFunc
}

// Sends one request on the live socket, answering with its future and the epoch of that socket
func (c *GatewayConnection) send(ctx context.Context, method string, budget time.Duration, params map[string]any) (*rpcFuture, int, error) {
	if c.onLoop(ctx) {
		return nil, 0, &LoopThreadError{Msg: "GatewayConnection.call() on the gateway loop thread would deadlock"}
	}
	if err := c.Start(ctx, DefaultStartTimeout); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	live, looping := c.live, c.looping
	c.mu.Unlock()
	if live == nil || !looping {
		return nil, 0, &gateway.GatewayClientError{Msg: gateway.GatewayNotSent}
	}
	rpcCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	future := &rpcFuture{done: make(chan rpcOutcome, 1), cancel: cancel}
	go func() {
		result, err := live.client.RPCTimeout(rpcCtx, method, params, budget)
		future.done <- rpcOutcome{result, err}
	}()
	return future, live.epoch, nil
}

// One request with the epoch of the socket that carried it, so admitted work is tied to that gateway life
func (c *GatewayConnection) CallTagged(ctx context.Context, method string, params map[string]any, o CallOptions) (any, int, error) {
	budget := o.Timeout
	if budget == 0 {
		budget = gateway.DefaultRPCTimeout
	}
	if o.Abandon != nil && o.Abandon() {
		return nil, 0, &CallStopped{Method: method}
	}
	future, epoch, err := c.send(ctx, method, budget, params)
	if err != nil {
		return nil, 0, err
	}
	defer future.cancel()
	unanswered := &gateway.GatewayClientError{Msg: fmt.Sprintf("Gateway did not answer %s", method)}
	settle := func(outcome rpcOutcome) (any, int, error) {
		if errors.Is(outcome.err, os.ErrDeadlineExceeded) {
			return nil, 0, unanswered
		}
		if outcome.err != nil {
			return nil, 0, outcome.err
		}
		return outcome.result, epoch, nil
	}
	var deadline <-chan time.Time
	var expires time.Time
	if budget > 0 {
		expires = time.Now().Add(budget + 5*time.Second)
		timer := time.NewTimer(budget + 5*time.Second)
		defer timer.Stop()
		deadline = timer.C
	}
	if o.Abandon != nil {
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		for expires.IsZero() || time.Now().Before(expires) {
			select {
			case outcome := <-future.done:
				return settle(outcome)
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-poll.C:
			}
			if o.Abandon() {
				return nil, 0, &CallStopped{Method: method}
			}
		}
	}
	select {
	case outcome := <-future.done:
		return settle(outcome)
	case <-deadline:
		return nil, 0, unanswered
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}

type commandAnswer struct {
	mu     sync.Mutex
	done   bool
	result any
	err    error
	wake   *pythread.Event
}

// Settles the answer once and wakes the waiting caller
func (a *commandAnswer) settle(result any, err error) {
	a.mu.Lock()
	a.done, a.result, a.err = true, result, err
	a.mu.Unlock()
	a.wake.Set()
}

func (a *commandAnswer) settled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done
}

// One request with no answer deadline on a socket of its own, given up with CallStopped once wake is set
func (c *GatewayConnection) CallUntil(ctx context.Context, method string, wake *pythread.Event, params map[string]any) (any, error) {
	if c.onLoop(ctx) {
		return nil, &LoopThreadError{Msg: "GatewayConnection.call_until() on the gateway loop thread would deadlock"}
	}
	if err := c.Start(ctx, DefaultStartTimeout); err != nil {
		return nil, err
	}
	if c.Final() {
		return nil, &gateway.GatewayClientError{Msg: gateway.GatewayNotSent}
	}
	answer := &commandAnswer{wake: wake}
	sent := &pythread.Event{}
	thread := memory.SpawnContextThread(ctx, "icebath-gateway-command", func(ctx context.Context) {
		c.command(ctx, method, params, wake, answer, sent)
	})
	c.sideLock.Lock()
	c.commands[wake] = true
	c.sideLock.Unlock()
	thread.Start()
	wake.Wait(-1)
	wake.Set()
	thread.Join(10 * time.Second)
	c.sideLock.Lock()
	delete(c.commands, wake)
	c.sideLock.Unlock()
	if answer.settled() {
		return answer.result, answer.err
	}
	if sent.IsSet() {
		return nil, &CallStopped{Method: method}
	}
	return nil, &gateway.GatewayClientError{Msg: gateway.GatewayNotSent}
}

// Command socket of one unbounded call, dial then send once then settle with the reply or the transport error
func (c *GatewayConnection) command(ctx context.Context, method string, params map[string]any, wake *pythread.Event, answer *commandAnswer, sent *pythread.Event) {
	connect := c.connect
	if connect == nil {
		connect = connectGateway
	}
	cmdCtx, cancel := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		wake.Wait(-1)
		if !answer.settled() {
			cancel()
		}
	}()
	defer func() {
		wake.Set()
		<-watched
		cancel()
	}()
	client, err := connect(cmdCtx)
	if err != nil {
		if cmdCtx.Err() != nil {
			return
		}
		if !isClientError(err) {
			err = &gateway.GatewayClientError{Msg: gateway.GatewayNotSent}
		}
		answer.settle(nil, err)
		return
	}
	defer client.Close()
	sent.Set()
	result, err := client.RPCTimeout(cmdCtx, method, params, -1)
	if err != nil {
		if cmdCtx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		if !isClientError(err) {
			err = &gateway.GatewayClientError{Msg: gateway.GatewayDisconnected}
		}
		answer.settle(nil, err)
		return
	}
	answer.settle(result, nil)
}
