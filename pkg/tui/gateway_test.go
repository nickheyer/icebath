package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Clock double whose timers fire only when a case advances it
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (k *fakeClock) Now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.now
}

func (k *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	tm := &fakeTimer{at: k.now.Add(d), f: f}
	k.timers = append(k.timers, tm)
	return func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		was := !tm.stopped
		tm.stopped = true
		return was
	}
}

// Active timers, the vi.getTimerCount of a case
func (k *fakeClock) pending() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, tm := range k.timers {
		if !tm.stopped {
			n++
		}
	}
	return n
}

// Whether a timer is due exactly after d from now
func (k *fakeClock) dueIn(d time.Duration) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, tm := range k.timers {
		if !tm.stopped && tm.at.Equal(k.now.Add(d)) {
			return true
		}
	}
	return false
}

// Moves time forward, firing every due timer in order and letting the goroutines it starts run
func (k *fakeClock) advance(d time.Duration) {
	k.mu.Lock()
	target := k.now.Add(d)
	k.mu.Unlock()
	for {
		k.mu.Lock()
		var next *fakeTimer
		for _, tm := range k.timers {
			if !tm.stopped && !tm.at.After(target) && (next == nil || tm.at.Before(next.at)) {
				next = tm
			}
		}
		if next == nil {
			k.now = target
			k.mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			return
		}
		next.stopped = true
		if next.at.After(k.now) {
			k.now = next.at
		}
		k.mu.Unlock()
		next.f()
		time.Sleep(2 * time.Millisecond)
	}
}

// WebSocket double, opened and closed by the case, recording what the client sent
type fakeSocket struct {
	url       string
	protocols []string
	inbox     chan string
	opened    chan struct{}
	done      chan struct{}
	openOnce  sync.Once
	closeOnce sync.Once

	mu      sync.Mutex
	sent    []string
	closed  bool
	code    int
	waiting bool
}

func (s *fakeSocket) Recv() (string, error) {
	s.mu.Lock()
	s.waiting = len(s.inbox) == 0
	s.mu.Unlock()
	select {
	case raw := <-s.inbox:
		s.mu.Lock()
		s.waiting = false
		s.mu.Unlock()
		return raw, nil
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return "", &websocket.CloseError{Code: s.code}
	}
}

func (s *fakeSocket) Send(data string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("socket not open")
	}
	s.sent = append(s.sent, data)
	return nil
}

func (s *fakeSocket) Open() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *fakeSocket) Subprotocol() string { return "" }

// Local close, which the reader sees as an abnormal close
func (s *fakeSocket) Close() error {
	s.closeWith(websocket.CloseAbnormalClosure)
	return nil
}

// Opens the socket, finishing the client's dial
func (s *fakeSocket) open() { s.openOnce.Do(func() { close(s.opened) }) }

// Closes the socket from the far side with a close code
func (s *fakeSocket) closeWith(code int) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed, s.code = true, code
		s.mu.Unlock()
		close(s.done)
	})
}

func (s *fakeSocket) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Delivers one inbound frame
func (s *fakeSocket) message(raw string) { s.inbox <- raw }

// Whether the reader consumed every inbound frame and waits for the next
func (s *fakeSocket) drained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox) == 0 && s.waiting
}

func (s *fakeSocket) sentFrames() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.sent))
	for _, raw := range s.sent {
		var frame map[string]any
		_ = json.Unmarshal([]byte(raw), &frame)
		out = append(out, frame)
	}
	return out
}

func (s *fakeSocket) sentRaw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// Last frame the client sent with the method, nil when none
func (s *fakeSocket) last(method string) map[string]any {
	frames := s.sentFrames()
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i]["method"] == method {
			return frames[i]
		}
	}
	return nil
}

func (s *fakeSocket) methods() []string {
	var out []string
	for _, f := range s.sentFrames() {
		m, _ := f["method"].(string)
		out = append(out, m)
	}
	return out
}

// Answers the last request of the method with a body of result or error
func (s *fakeSocket) answer(method string, body map[string]any) {
	frame := s.last(method)
	out := map[string]any{"jsonrpc": "2.0", "id": frame["id"]}
	for k, v := range body {
		out[k] = v
	}
	raw, _ := json.Marshal(out)
	s.message(string(raw))
}

// Dialer double, each dial a new socket that connects once the case opens it
type fakeDialer struct {
	mu        sync.Mutex
	instances []*fakeSocket
}

func (d *fakeDialer) dial(ctx context.Context, target string, protocols []string) (gateway.GatewaySocket, error) {
	s := &fakeSocket{url: target, protocols: protocols, inbox: make(chan string, 64), opened: make(chan struct{}), done: make(chan struct{})}
	d.mu.Lock()
	d.instances = append(d.instances, s)
	d.mu.Unlock()
	select {
	case <-s.opened:
		return s, nil
	case <-s.done:
		return nil, errors.New("websocket closed during connect")
	case <-ctx.Done():
		s.closeWith(websocket.CloseAbnormalClosure)
		return nil, ctx.Err()
	}
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.instances)
}

func (d *fakeDialer) at(i int) *fakeSocket {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i < 0 {
		i += len(d.instances)
	}
	return d.instances[i]
}

// Subscriber double recording every message in order
type recorder struct {
	mu   sync.Mutex
	msgs []tea.Msg
	hook func(tea.Msg)
}

func (r *recorder) send(m tea.Msg) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	hook := r.hook
	r.mu.Unlock()
	if hook != nil {
		hook(m)
	}
}

func (r *recorder) all() []tea.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]tea.Msg(nil), r.msgs...)
}

func (r *recorder) events(kind string) []gatewayEventMsg {
	var out []gatewayEventMsg
	for _, m := range r.all() {
		if ev, isEvent := m.(gatewayEventMsg); isEvent && (kind == "" || ev.Type == kind) {
			out = append(out, ev)
		}
	}
	return out
}

func (r *recorder) exits() []int {
	var out []int
	for _, m := range r.all() {
		if ex, isExit := m.(gatewayExitMsg); isExit {
			out = append(out, ex.Code)
		}
	}
	return out
}

// Bootstrap double answering scripted outcomes in order, the last one repeating
type fakeBootstrap struct {
	mu       sync.Mutex
	outcomes []error
	calls    [][2]bool
}

var testGrant = localGrant{URL: "ws://gateway.test/api/ws", ProfileID: "fixture", InstanceID: "owner"}

func (b *fakeBootstrap) run(_ context.Context, start, recover bool) (localGrant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, [2]bool{start, recover})
	var err error
	if len(b.outcomes) > 0 {
		err = b.outcomes[0]
		if len(b.outcomes) > 1 {
			b.outcomes = b.outcomes[1:]
		}
	}
	if err != nil {
		return localGrant{}, err
	}
	return testGrant, nil
}

func (b *fakeBootstrap) callList() [][2]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([][2]bool(nil), b.calls...)
}

// Client over the doubles, killed when the case ends
func newTestClient(t *testing.T, boot *fakeBootstrap) (*gatewayClient, *fakeDialer, *fakeClock) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if boot == nil {
		boot = &fakeBootstrap{}
	}
	d, k := &fakeDialer{}, newFakeClock()
	c := &gatewayClient{bootstrap: boot.run, dial: d.dial, clock: k, env: os.Getenv, timeout: requestTimeout()}
	t.Cleanup(func() { c.kill("test") })
	return c, d, k
}

// Waits for a condition the client reaches on its own goroutines
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Outcome of a request started on its own goroutine
type pendingResult struct {
	done  chan struct{}
	value any
	err   error
}

func (p *pendingResult) wait(t *testing.T) (any, error) {
	t.Helper()
	select {
	case <-p.done:
		return p.value, p.err
	case <-time.After(5 * time.Second):
		t.Fatal("request never settled")
		return nil, nil
	}
}

func (p *pendingResult) settled() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func goRequest(c *gatewayClient, method string, params map[string]any) *pendingResult {
	p := &pendingResult{done: make(chan struct{})}
	go func() {
		p.value, p.err = c.request(context.Background(), method, params, 0)
		close(p.done)
	}()
	return p
}

// Explicit attach whose socket is open and drained
func openExplicit(t *testing.T, url string) (*gatewayClient, *fakeDialer, *fakeClock, *fakeSocket) {
	t.Helper()
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", url)
	t.Setenv("ICEBATH_TUI_SIDECAR_URL", "")
	c, d, k := newTestClient(t, nil)
	c.start()
	eventually(t, "the socket", func() bool { return d.count() == 1 })
	s := d.at(0)
	s.open()
	eventually(t, "the open socket", func() bool { return s.drained() })
	return c, d, k, s
}

func TestGatewayClientRequests(t *testing.T) {
	t.Run("answers decode into the JavaScript data model", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws")
		p := goRequest(c, "session.info", map[string]any{"session_id": "s"})
		eventually(t, "the request", func() bool { return s.last("session.info") != nil })
		s.answer("session.info", map[string]any{"result": map[string]any{"ok": true, "n": 3}})
		got, err := p.wait(t)
		res, _ := got.(map[string]any)
		if err != nil || res["ok"] != true || res["n"] != float64(3) {
			t.Fatalf("got %#v %v", got, err)
		}
	})
	t.Run("refusals carry the server code and message", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws")
		p := goRequest(c, "session.resume", nil)
		eventually(t, "the request", func() bool { return s.last("session.resume") != nil })
		s.answer("session.resume", map[string]any{"error": map[string]any{"code": 4001, "message": "session not found"}})
		_, err := p.wait(t)
		var refusal *rpcError
		if !errors.As(err, &refusal) || !refusal.HasCode || refusal.Code != 4001 || refusal.Message != "session not found" {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("an unanswered request times out naming the method", func(t *testing.T) {
		c, _, _, _ := openExplicit(t, "ws://gateway.test/api/ws")
		_, err := c.request(context.Background(), "a.slow", nil, time.Second)
		if err == nil || err.Error() != "request timed out after 1s: a.slow" {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("a request after kill is not connected", func(t *testing.T) {
		c, _, _, _ := openExplicit(t, "ws://gateway.test/api/ws")
		c.kill("test")
		if _, err := c.request(context.Background(), "x.y", nil, 0); err == nil || err.Error() != "gateway not connected: x.y" {
			t.Fatalf("err %v", err)
		}
		if !strings.Contains(c.logTail(5), "[lifecycle] start() ignored after kill()") {
			t.Fatalf("logs %q", c.logTail(5))
		}
	})
	t.Run("the timeout floor is thirty seconds", func(t *testing.T) {
		t.Setenv("ICEBATH_TUI_RPC_TIMEOUT_MS", "5")
		if requestTimeout() != 30*time.Second {
			t.Fatal("floor")
		}
		t.Setenv("ICEBATH_TUI_RPC_TIMEOUT_MS", "")
		if requestTimeout() != 120*time.Second {
			t.Fatal("default")
		}
		t.Setenv("ICEBATH_TUI_RPC_TIMEOUT_MS", " 45000ms")
		if requestTimeout() != 45*time.Second {
			t.Fatal("parseInt reads the leading digits")
		}
		t.Setenv("ICEBATH_TUI_STARTUP_TIMEOUT_MS", "100")
		if startupTimeout() != 5*time.Second {
			t.Fatal("startup floor")
		}
		t.Setenv("ICEBATH_TUI_STARTUP_TIMEOUT_MS", "")
		if startupTimeout() != 15*time.Second {
			t.Fatal("startup default")
		}
	})
}

func TestGatewayClientFrames(t *testing.T) {
	t.Run("events and server requests buffer until a program attaches, then the transport exit arrives", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws")
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"message.delta","session_id":"s1","payload":{"text":"hi"}}}`)
		s.message(`{"jsonrpc":"2.0","id":"srv1","method":"clarify","params":{"question":"why"}}`)
		eventually(t, "the frames", func() bool { return s.drained() })
		s.closeWith(1011)
		eventually(t, "the exit", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.pendingExit != nil })
		rec := &recorder{}
		c.attach(rec.send)
		eventually(t, "the flush", func() bool { return len(rec.all()) == 4 })
		got := rec.all()
		ev, isEvent := got[0].(gatewayEventMsg)
		if !isEvent || ev.Type != "message.delta" || ev.SessionID != "s1" || ev.Payload["text"] != "hi" {
			t.Fatalf("event %#v", got[0])
		}
		if rc, isEvent := got[1].(gatewayEventMsg); !isEvent || rc.Type != "gateway.reconnecting" {
			t.Fatalf("reconnecting %#v", got[1])
		}
		req, isReq := got[2].(serverRequestMsg)
		if !isReq || req.Request.ID != "srv1" || req.Request.Method != "clarify" || req.Request.Params["question"] != "why" {
			t.Fatalf("request %#v", got[2])
		}
		if ex, isExit := got[3].(gatewayExitMsg); !isExit || ex.Code != 1011 {
			t.Fatalf("exit %#v", got[3])
		}
	})
	t.Run("the backlog keeps the newest two thousand frames", func(t *testing.T) {
		c, _, _ := newTestClient(t, nil)
		c.mu.Lock()
		for i := 0; i < maxBufferedEvents+5; i++ {
			c.publishLocked(gatewayEventMsg{Type: "status.update", Payload: map[string]any{"i": i}})
		}
		n, first := len(c.backlog), c.backlog[0].(gatewayEventMsg).Payload["i"]
		c.mu.Unlock()
		if n != maxBufferedEvents || first != 5 {
			t.Fatalf("backlog %d first %v", n, first)
		}
	})
	t.Run("answers to server requests echo the id over the socket once", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws")
		rec := &recorder{}
		c.attach(rec.send)
		s.message(`{"jsonrpc":"2.0","id":"srv1","method":"clarify","params":{}}`)
		s.message(`{"jsonrpc":"2.0","id":"srv2","method":"approval.request","params":{}}`)
		var reqs []*serverRequest
		eventually(t, "the requests", func() bool {
			reqs = nil
			for _, m := range rec.all() {
				if r, isReq := m.(serverRequestMsg); isReq {
					reqs = append(reqs, r.Request)
				}
			}
			return len(reqs) == 2
		})
		reqs[0].respond(map[string]any{"answer": "a"})
		reqs[0].respond(map[string]any{"answer": "b"})
		reqs[1].fail(4009, "cancelled")
		sent := s.sentRaw()
		if len(sent) != 2 || sent[0] != `{"jsonrpc":"2.0","id":"srv1","result":{"answer":"a"}}` || sent[1] != `{"jsonrpc":"2.0","id":"srv2","error":{"code":4009,"message":"cancelled"}}` {
			t.Fatalf("sent %q", sent)
		}
	})
	t.Run("the log ring keeps two hundred lines of at most four kilobytes with the truncation marker", func(t *testing.T) {
		c, _, _ := newTestClient(t, nil)
		for i := 0; i < maxGatewayLogLines+3; i++ {
			c.recordLog(strings.Repeat("x", maxLogLineBytes+10))
		}
		want := strings.Repeat("x", maxLogLineBytes) + fmt.Sprintf("… [truncated %d bytes]", maxLogLineBytes+10)
		if len(c.logs) != maxGatewayLogLines || c.logs[0] != want {
			t.Fatalf("logs %d %q", len(c.logs), c.logs[0][maxLogLineBytes:])
		}
	})
}

func TestGatewayClientProtocolError(t *testing.T) {
	t.Run("a frame that is not an object is logged and published as gateway.protocol_error, the socket staying up", func(t *testing.T) {
		c, _, _, s := openExplicit(t, "ws://gateway.test/api/ws")
		rec := &recorder{}
		c.attach(rec.send)
		s.message(`["x"]`)
		s.message(``)
		s.message(`{"jsonrpc":"2.0","method":"event","params":{"type":"session.info","payload":{}}}`)
		eventually(t, "the events", func() bool { return len(rec.events("")) == 2 })
		evs := rec.events("")
		if evs[0].Type != "gateway.protocol_error" || evs[0].Payload["preview"] != `["x"]` || evs[1].Type != "session.info" {
			t.Fatalf("events %#v", evs)
		}
		if !strings.Contains(c.logTail(5), `[protocol] malformed websocket frame: ["x"]`) || s.isClosed() {
			t.Fatalf("logs %q closed %v", c.logTail(5), s.isClosed())
		}
	})
}
