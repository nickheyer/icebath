package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pywebsockets"
	"github.com/nickheyer/icebath/pkg/tools"
)

func runtimeSessionStoreLog() *slog.Logger {
	return slog.Default().With("logger", "agent.runtime_session_store")
}

var workerProcess atomic.Bool

// Whether a successful self registration made this process compute only, process scoped and never per goroutine
func IsWorkerProcess() bool { return workerProcess.Load() }

// Python exception a worker store raises that is not a WorkerPersistenceError
type workerStoreError struct{ kind, msg string }

func (e *workerStoreError) Error() string      { return e.msg }
func (e *workerStoreError) PyTypeName() string { return e.kind }

// Mirrors the owner's in place row stamps onto the worker's dicts, a None annotation popping the key
func ApplyRowAnnotations(messages []map[string]any, annotations any) error {
	list, ok := annotations.([]any)
	if !ok {
		return &workerStoreError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(annotations))}
	}
	for i := range max(len(messages), len(list)) {
		if i >= len(messages) {
			return &workerStoreError{"ValueError", "zip() argument 2 is longer than argument 1"}
		}
		if i >= len(list) {
			return &workerStoreError{"ValueError", "zip() argument 2 is shorter than argument 1"}
		}
		annotation, ok := list[i].(*pyjson.Object)
		if !ok {
			return &workerStoreError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'items'", pyrepr.TypeName(list[i]))}
		}
		for _, key := range annotation.Keys {
			value := annotation.Vals[key]
			if value == nil {
				delete(messages[i], key)
			} else {
				messages[i][key] = pyjson.Plain(value)
			}
		}
	}
	return nil
}

// package:function:line of the frames above this store, its own runtime_session files skipped
func callerSummary(limit int) string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for len(out) < limit {
		frame, more := frames.Next()
		if base := filepath.Base(frame.File); !strings.HasPrefix(base, "runtime_session_") || strings.HasSuffix(base, "_test.go") {
			name := frame.Function
			module, fn := "?", name
			if dot := strings.LastIndex(name[strings.LastIndex(name, "/")+1:], "."); dot >= 0 {
				cut := strings.LastIndex(name, "/") + 1 + dot
				module, fn = name[:cut], name[cut+1:]
			}
			out = append(out, fmt.Sprintf("%s:%s:%d", module, fn, frame.Line))
		}
		if !more {
			break
		}
	}
	return strings.Join(out, " <- ")
}

// Name of the goroutine running this call, the Go spelling of the current thread name
func currentThreadName() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	if line, _, ok := bytes.Cut(buf, []byte(" [")); ok {
		return string(line)
	}
	return "goroutine"
}

// Runs fn here, or on a fresh goroutine without the loop marker when this context runs an event loop
func CallOffEventLoop(ctx context.Context, fn func(ctx context.Context) error) error {
	if !pyctx.RunningLoop(ctx) && !tools.RelayHasRunningEventLoop(ctx) {
		return fn(ctx)
	}
	done := make(chan error, 1)
	off := tools.RelayWithoutRunningLoop(pyctx.WithoutRunningLoop(ctx))
	go func() { done <- fn(off) }()
	return <-done
}

// One owner WebSocket and the only goroutine that reads it, the awaited reply handed to its caller
type ownerLink struct {
	ws       *websocket.Conn
	awaiting atomic.Value
	mu       sync.Mutex
	replies  []any
	ready    chan struct{}
	done     chan struct{}
}

// Link over an open socket whose reader goroutine starts at once, no request awaited yet
func newOwnerLink(ws *websocket.Conn) *ownerLink {
	l := &ownerLink{ws: ws, ready: make(chan struct{}, 1), done: make(chan struct{})}
	l.awaiting.Store(int64(0))
	go l.read()
	return l
}

// Appends to the unbounded reply queue so the reader never blocks, waking a waiting receive
func (l *ownerLink) put(reply any) {
	l.mu.Lock()
	l.replies = append(l.replies, reply)
	l.mu.Unlock()
	select {
	case l.ready <- struct{}{}:
	default:
	}
}

// Oldest queued reply, false when the queue is empty
func (l *ownerLink) pop() (any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.replies) == 0 {
		return nil, false
	}
	reply := l.replies[0]
	l.replies[0] = nil
	l.replies = l.replies[1:]
	return reply, true
}

// Discards broadcast events as they arrive and queues the reply to the awaited request id or the read error
func (l *ownerLink) read() {
	defer close(l.done)
	for {
		_, data, err := l.ws.ReadMessage()
		if err != nil {
			l.put(err)
			return
		}
		frame, err := pyjson.LoadsBytes(data)
		if err != nil {
			continue
		}
		obj, ok := frame.(*pyjson.Object)
		awaiting := l.awaiting.Load().(int64)
		if ok && awaiting != 0 && pyjson.Equal(obj.GetOr("id", nil), awaiting) {
			l.put(obj)
		}
	}
}

// Whether the reader goroutine is still running
func (l *ownerLink) alive() bool {
	select {
	case <-l.done:
		return false
	default:
		return true
	}
}

func (l *ownerLink) send(requestID int64, message string) error {
	l.awaiting.Store(requestID)
	return l.ws.WriteMessage(websocket.TextMessage, []byte(message))
}

// Next reply within the timeout, worker_receipt_timeout once it lapses and the read error as raised
func (l *ownerLink) receive(timeout time.Duration) (*pyjson.Object, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if reply, ok := l.pop(); ok {
			if err, isErr := reply.(error); isErr {
				return nil, err
			}
			return reply.(*pyjson.Object), nil
		}
		select {
		case <-l.ready:
		case <-timer.C:
			return nil, &workerStoreError{"TimeoutError", "worker_receipt_timeout"}
		}
	}
}

// Closes the socket with a close handshake bounded at one second
func (l *ownerLink) close() error {
	_ = l.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return l.ws.Close()
}

// Bounded authenticated calls to an existing owner over one reused connection, never starting a daemon
type WorkerRPC struct {
	home      string
	mu        sync.Mutex
	endpoint  *gateway.GatewayEndpoint
	link      *ownerLink
	requestID int64
}

// Caller bound to the profile home, resolved
func NewWorkerRPC(home string) (*WorkerRPC, error) {
	resolved, err := pypath.Resolve(home)
	if err != nil {
		return nil, err
	}
	return &WorkerRPC{home: resolved}, nil
}

// Live link, redialed with a fresh worker adoption ticket after the owner closed it while idle
func (r *WorkerRPC) connection(ctx context.Context) (*ownerLink, error) {
	if r.link != nil {
		if r.link.alive() {
			return r.link, nil
		}
		r.drop()
	}
	discovery := gateway.DiscoverGatewayEndpoint(ctx, r.home, 5*time.Second)
	if discovery.State == "ready" && discovery.Endpoint != nil {
		r.endpoint = discovery.Endpoint
	} else if discovery.State != "draining" || r.endpoint == nil {
		return nil, tools.NewWorkerPersistenceError("owner_unavailable")
	}
	endpoint := r.endpoint
	descriptor, err := gateway.QueryIdentify(gateway.ControlHomeFor(r.home, endpoint), 5*time.Second)
	if err != nil {
		return nil, err
	}
	if pyjson.Equal(descriptor.GetOr("pid", nil), int64(os.Getpid())) {
		return nil, tools.NewWorkerPersistenceError("synchronous_self_rpc")
	}
	ticket, err := gateway.SessionTicket(r.home, endpoint, "worker-adoption")
	if err != nil {
		return nil, err
	}
	url, protocols := gateway.GatewayWSTarget(endpoint, ticket)
	dialer := websocket.Dialer{Proxy: nil, HandshakeTimeout: 5 * time.Second, Subprotocols: protocols}
	ws, resp, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return nil, pywebsockets.HandshakeError(resp, err)
	}
	ws.SetReadLimit(8 * 1024 * 1024)
	r.link = newOwnerLink(ws)
	return r.link, nil
}

// Send and receive half of the owner connection one request travels over
type workerOwnerConn interface {
	send(requestID int64, message string) error
	receive(timeout time.Duration) (*pyjson.Object, error)
}

// Live owner connection of an exchange, the WorkerRPC connection hermes tests swap for an in process reply
var workerRPCConnection = func(r *WorkerRPC, ctx context.Context) (workerOwnerConn, error) {
	link, err := r.connection(ctx)
	if err != nil {
		return nil, err
	}
	return link, nil
}

// Forgets the link, closing a socket that is not already gone
func (r *WorkerRPC) drop() {
	link := r.link
	r.link = nil
	if link != nil {
		_ = link.close()
	}
}

// Closes the connection under the call lock
func (r *WorkerRPC) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drop()
}

// One JSON-RPC call to the owner, refused on an event loop and never resent once its outcome is unknown
func (r *WorkerRPC) Call(ctx context.Context, method string, params *pyjson.Object) (any, error) {
	if pyctx.RunningLoop(ctx) {
		runtimeSessionStoreLog().Warn(fmt.Sprintf("synchronous_rpc_on_event_loop: %s on thread %s; caller: %s", method, currentThreadName(), callerSummary(8)))
		return nil, tools.NewWorkerPersistenceError("synchronous_rpc_on_event_loop")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requestID++
	requestID := r.requestID
	if params == nil {
		params = pyjson.NewObject()
	}
	message, err := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", requestID, "method", method, "params", params))
	if err != nil {
		return nil, err
	}
	response, err := r.exchange(ctx, requestID, message)
	if err != nil {
		r.drop()
		return nil, err
	}
	if raw, has := response.Get("error"); has {
		failure, ok := raw.(*pyjson.Object)
		if !ok {
			return nil, &workerStoreError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(raw))}
		}
		reason := failure.GetOr("message", "persistence_failed")
		text, isText := reason.(string)
		if !isText {
			text = pyrepr.Str(reason)
		}
		return nil, tools.NewWorkerPersistenceError(text)
	}
	if method == "worker.register" || method == "worker.adopt" {
		if pyjson.Equal(params.GetOr("pid", nil), int64(os.Getpid())) {
			workerProcess.Store(true)
		}
	}
	result, has := response.Get("result")
	if !has {
		return nil, &workerStoreError{"KeyError", "'result'"}
	}
	return result, nil
}

// Sends the request once, redialing a single time when the send itself failed, then waits for its reply
func (r *WorkerRPC) exchange(ctx context.Context, requestID int64, message string) (*pyjson.Object, error) {
	link, err := workerRPCConnection(r, ctx)
	if err != nil {
		return nil, err
	}
	if err := link.send(requestID, message); err != nil {
		r.drop()
		if link, err = workerRPCConnection(r, ctx); err != nil {
			return nil, err
		}
		if err := link.send(requestID, message); err != nil {
			return nil, err
		}
	}
	return link.receive(20 * time.Second)
}
