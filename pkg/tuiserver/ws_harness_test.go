package tuiserver

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Scripted socket of the ws cases, frames pushed on inbound and a nil frame hanging up
type wsTestSocket struct {
	mu        sync.Mutex
	sent      []map[string]any
	lines     []string
	inbound   chan any
	received  int
	failIDs   map[any]bool
	authority *gateway.SessionAuthority
	raw       net.Conn
	onReceive func()
	accepted  []*string
	closes    int
}

// Socket of the cases, admitted with a session authority like every socket the service serves
func newWSTestSocket() *wsTestSocket {
	return &wsTestSocket{inbound: make(chan any, 4096), failIDs: map[any]bool{}, authority: new(gateway.SessionAuthority)}
}

// Authority connection of the socket cases, each request answered by dispatch and as an unknown method without one
type wsTestConnection struct {
	dispatch func(ctx context.Context, request *jsonutil.Object) (*jsonutil.Object, error)
}

func (c *wsTestConnection) Dispatch(ctx context.Context, request *jsonutil.Object) (*jsonutil.Object, error) {
	if c.dispatch != nil {
		return c.dispatch(ctx, request)
	}
	return jsonutil.ObjectOf("jsonrpc", "2.0", "id", request.ValueOr("id", nil),
		"error", jsonutil.ObjectOf("code", -32601, "message", "unknown method")), nil
}
func (c *wsTestConnection) Close(context.Context) error { return nil }
func (c *wsTestConnection) Actor() gateway.Principal    { return gateway.Principal{} }
func (c *wsTestConnection) AwaitsProfileStart(context.Context, map[string]any, []string) (string, <-chan struct{}, bool, error) {
	return "", nil, false, nil
}

// Answers the requests of every socket of one case with dispatch in place of a real authority connection
func wsTestFakeAuthority(t *testing.T, dispatch func(ctx context.Context, request *jsonutil.Object) (*jsonutil.Object, error)) {
	t.Helper()
	prev := wsNewAuthorityConnection
	wsNewAuthorityConnection = func(*gateway.SessionAuthority, *tuihost.WSTransport, map[string]any, bool) (authorityConn, error) {
		return &wsTestConnection{dispatch: dispatch}, nil
	}
	t.Cleanup(func() { wsNewAuthorityConnection = prev })
}

func (s *wsTestSocket) Accept(_ context.Context, subprotocol *string) error {
	s.mu.Lock()
	s.accepted = append(s.accepted, subprotocol)
	s.mu.Unlock()
	return nil
}

func (s *wsTestSocket) SendText(_ context.Context, text string) error {
	for _, part := range strings.Split(text, "\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		decoded, err := jsonutil.Decode(part)
		if err != nil {
			return err
		}
		frame, _ := jsonutil.Plain(decoded).(map[string]any)
		s.mu.Lock()
		if s.failIDs[frame["id"]] {
			s.mu.Unlock()
			return &gateway.PyError{Kind: "RuntimeError", Msg: "peer gone"}
		}
		s.sent = append(s.sent, frame)
		s.lines = append(s.lines, part)
		s.mu.Unlock()
	}
	return nil
}

func (s *wsTestSocket) ReceiveText(ctx context.Context) (string, error) {
	if s.onReceive != nil {
		s.onReceive()
	}
	select {
	case item := <-s.inbound:
		if item == nil {
			return "", &SocketDisconnect{Code: 1000}
		}
		s.mu.Lock()
		s.received++
		s.mu.Unlock()
		if text, isText := item.(string); isText {
			return text, nil
		}
		text, err := jsonutil.Encode(item)
		return text, err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *wsTestSocket) Close(int) error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	return nil
}

func (s *wsTestSocket) Client() *SocketPeer                         { return nil }
func (s *wsTestSocket) RawConn() net.Conn                           { return s.raw }
func (s *wsTestSocket) SessionAuthority() *gateway.SessionAuthority { return s.authority }

// Frames the peer received so far
func (s *wsTestSocket) frames() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.sent...)
}

// Ids of the frames the peer received that carry one
func (s *wsTestSocket) ids() []any {
	var out []any
	for _, frame := range s.frames() {
		if id, has := frame["id"]; has {
			out = append(out, id)
		}
	}
	return out
}

func (s *wsTestSocket) receivedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

// JSON RPC request frame of the cases
func wsTestRPC(id any, method string) *jsonutil.Object {
	return jsonutil.ObjectOf("jsonrpc", "2.0", "id", id, "method", method, "params", jsonutil.NewObject())
}

// Polls a predicate until it holds or the timeout passes
func wsTestSettle(predicate func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !predicate() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// Position of a value in a list, minus one when absent
func wsTestIndex[T comparable](items []T, want T) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func wsTestHasID(ids []any, want any) bool { return wsTestIndex(ids, want) >= 0 }

// Parks orphaned sessions without a reap timer and keeps the process wide watchers from starting in one case
func wsTestNoOrphanGrace(t *testing.T) {
	prev, prevSkin, prevLease, prevLiveness := wsOrphanReapGraceS, wsEnsureSkinWatcher, wsEnsureLeaseWatcher, wsStartBackendLiveness
	wsOrphanReapGraceS = func() float64 { return 0 }
	wsEnsureSkinWatcher, wsEnsureLeaseWatcher, wsStartBackendLiveness = func(context.Context) {}, func() {}, func(context.Context) {}
	t.Cleanup(func() {
		wsOrphanReapGraceS, wsEnsureSkinWatcher, wsEnsureLeaseWatcher, wsStartBackendLiveness = prev, prevSkin, prevLease, prevLiveness
	})
}

// Registers a method for one case and restores the registry after it
func wsTestMethod(t *testing.T, name string, fn Handler) {
	t.Helper()
	prev := lookupMethod(name)
	registerMethod(name, fn)
	t.Cleanup(func() {
		methodsMu.Lock()
		defer methodsMu.Unlock()
		if prev == nil {
			delete(methods, name)
			return
		}
		methods[name] = prev
	})
}

// Event log the fakes of one case append to
type wsTestLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *wsTestLog) add(entry string) {
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *wsTestLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func (l *wsTestLog) has(entry string) bool { return wsTestIndex(l.snapshot(), entry) >= 0 }

func (l *wsTestLog) count(prefix string) int {
	n := 0
	for _, entry := range l.snapshot() {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

// Runs one socket session on its own goroutine and returns the channel its result arrives on
func wsTestServe(ctx context.Context, sock *wsTestSocket, identity map[string]any, operator bool) <-chan error {
	done := make(chan error, 1)
	go func() { done <- HandleWS(ctx, sock, identity, nil, operator) }()
	return done
}

// Waits for a served session to end within the timeout
func wsTestAwait(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("the socket session did not end in time")
		return nil
	}
}

// Transport the session of a connected socket registered for global broadcasts, the newest one
func wsTestLiveTransport(t *testing.T) *tuihost.WSTransport {
	t.Helper()
	live := tuihost.LiveTransports()
	for i := len(live) - 1; i >= 0; i-- {
		if transport, isWS := live[i].(*tuihost.WSTransport); isWS {
			return transport
		}
	}
	t.Fatal("no socket transport is registered")
	return nil
}
