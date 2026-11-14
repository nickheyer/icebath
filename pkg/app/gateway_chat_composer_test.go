package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Buffer safe to read while the chat goroutines write it
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Captures stdout and stderr into buffers the case may read while the chat runs
func captureStreamsLocked(t *testing.T) (*syncBuffer, *syncBuffer) {
	t.Helper()
	out, errOut := &syncBuffer{}, &syncBuffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out, StdinTTY: func() bool { return false },
		StdoutTTY: func() bool { return false }}, errOut))
	return out, errOut
}

// Feeds the composer keys through a pipe and discards its drawing, the write end returned
func pipeComposer(t *testing.T) func(keys string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	prevIn, prevOut := tuiStdin, tuiStdout
	tuiStdin, tuiStdout = func() *os.File { return reader }, func() *os.File { return devnull }
	t.Cleanup(func() {
		tuiStdin, tuiStdout = prevIn, prevOut
		writer.Close()
		reader.Close()
		devnull.Close()
	})
	return func(keys string) {
		if _, err := writer.WriteString(keys); err != nil {
			t.Error(err)
		}
	}
}

// Owner on a loopback websocket answering each request through answer, every request recorded
type wsOwner struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []fakeGatewayCall
	conns  []*websocket.Conn
}

func newWSOwner(t *testing.T, answer func(method string, params map[string]any, calls []fakeGatewayCall) any) *wsOwner {
	o := &wsOwner{}
	upgrader := websocket.Upgrader{}
	o.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		o.mu.Lock()
		o.conns = append(o.conns, ws)
		o.mu.Unlock()
		defer ws.Close()
		for {
			_, raw, err := ws.ReadMessage()
			if err != nil {
				return
			}
			decoded, _ := pyjson.Loads(string(raw))
			request, _ := decoded.(*pyjson.Object)
			params, _ := pyjson.Plain(request.Vals["params"]).(map[string]any)
			method, _ := request.Vals["method"].(string)
			o.mu.Lock()
			o.calls = append(o.calls, fakeGatewayCall{method, params})
			calls := append([]fakeGatewayCall(nil), o.calls...)
			o.mu.Unlock()
			result := wireValue(t, answer(method, params, calls))
			reply, _ := pyjson.Dumps(pyjson.ObjectOf("jsonrpc", "2.0", "id", request.Vals["id"], "result", result))
			if ws.WriteMessage(websocket.TextMessage, []byte(reply)) != nil {
				return
			}
		}
	}))
	t.Cleanup(o.server.Close)
	t.Setenv("ICEBATH_TUI_GATEWAY_URL", "ws"+strings.TrimPrefix(o.server.URL, "http"))
	return o
}

// Requests recorded so far
func (o *wsOwner) recorded() []fakeGatewayCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]fakeGatewayCall(nil), o.calls...)
}

// Drops every attached connection the way a dying owner does
func (o *wsOwner) die() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ws := range o.conns {
		ws.Close()
	}
	o.server.CloseClientConnections()
}

// Runs the chat for the namespace through the real connector, its result arriving on the channel
func runChatAsync(args map[string]any) (<-chan error, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runGatewayChat(ctx, args, nil, connectGatewayChat)
		done <- err
	}()
	return done, cancel
}

func TestIdleComposerExitsWithUnknownOutcomeWhenOwnerDiesWithoutKeypress(t *testing.T) {
	tempHome(t)
	captureStreamsLocked(t)
	pipeComposer(t)
	snapshot := map[string]any{"stored_session_id": "stored", "execution_generation": 1, "pending": []any{}}
	owner := newWSOwner(t, func(method string, params map[string]any, calls []fakeGatewayCall) any {
		if method == "session.resume" {
			return snapshot
		}
		return map[string]any{}
	})
	done, cancel := runChatAsync(map[string]any{"resume": "stored"})
	defer cancel()
	gatewayWaitUntil(t, func() bool { return len(owner.recorded()) >= 2 }, "the attach")
	time.Sleep(200 * time.Millisecond)
	owner.die()
	select {
	case err := <-done:
		var refused *gateway.GatewayClientError
		if !errors.As(err, &refused) || !strings.Contains(refused.Msg, "outcome is unknown") {
			t.Fatalf("err %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the idle composer kept waiting for a keypress")
	}
}

func TestResumeDisplaysUnknownAndQueueThenDiscardsWithRefreshedGeneration(t *testing.T) {
	tempHome(t)
	out, errOut := captureStreamsLocked(t)
	keys := pipeComposer(t)
	unknownID := "admission-unknown-0123456789abcdef0123456789abcdef"
	queuedID := "admission-queued-fedcba9876543210fedcba9876543210"
	snapshot := map[string]any{"stored_session_id": "stored", "execution_generation": 9, "pending": []any{
		map[string]any{"admission_id": unknownID, "status": "unknown", "execution_generation": 4},
		map[string]any{"admission_id": queuedID, "status": "queued", "execution_generation": nil}}}
	refreshed := map[string]any{"stored_session_id": "stored", "execution_generation": 10, "pending": []any{
		map[string]any{"admission_id": unknownID, "status": "unknown", "execution_generation": 5},
		map[string]any{"admission_id": queuedID, "status": "queued", "execution_generation": nil}}}
	owner := newWSOwner(t, func(method string, params map[string]any, calls []fakeGatewayCall) any {
		if method != "session.resume" {
			return map[string]any{}
		}
		resumes := 0
		for _, c := range calls {
			if c.method == "session.resume" {
				resumes++
			}
		}
		if resumes == 1 {
			return snapshot
		}
		return refreshed
	})
	done, cancel := runChatAsync(map[string]any{"resume": "stored"})
	defer cancel()
	gatewayWaitUntil(t, func() bool { return len(owner.recorded()) >= 2 }, "the attach")
	time.Sleep(200 * time.Millisecond)
	var commands []string
	for _, line := range strings.Split(errOut.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/discard ") {
			commands = append(commands, strings.TrimSpace(line))
		}
	}
	if len(commands) != 1 || commands[0] != "/discard "+unknownID {
		t.Fatalf("discard commands %q in %q", commands, errOut.String())
	}
	text := errOut.String()
	if !strings.Contains(text, "This chat is paused") || !slicesContains(strings.Split(text, "\n"), "    (queued message "+queuedID+")") ||
		!strings.Contains(strings.ToLower(text), "waiting") || !strings.Contains(strings.ToLower(text), "won't re-run") {
		t.Fatalf("stderr %q", text)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 1 || lines[0] != "Welcome to Icebath Agent! Type your message or /help for commands." {
		t.Fatalf("stdout %q", out.String())
	}
	if calls := owner.recorded(); len(calls) != 2 || calls[0].method != "runtime.describe" || len(calls[0].params) != 0 ||
		calls[1].method != "session.resume" || len(calls[1].params) != 1 || calls[1].params["session_id"] != "stored" {
		t.Fatalf("calls before the discard %v", calls)
	}
	keys(commands[0] + "\r")
	gatewayWaitUntil(t, func() bool { return len(owner.recorded()) >= 4 }, "the discard")
	time.Sleep(200 * time.Millisecond)
	keys("/quit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the chat did not detach")
	}
	calls := owner.recorded()
	resolve := calls[len(calls)-1]
	if len(calls) != 4 || calls[2].method != "session.resume" || calls[2].params["session_id"] != "stored" ||
		resolve.method != "prompt.resolve_unknown" || len(resolve.params) != 3 || resolve.params["session_id"] != "stored" ||
		resolve.params["admission_id"] != unknownID || resolve.params["execution_generation"] != float64(5) && resolve.params["execution_generation"] != int64(5) {
		t.Fatalf("calls %v", calls)
	}
}

// Whether the list holds the value
func slicesContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestTheClassicChatViewReadsNoPluginManager(t *testing.T) {
	captureStreamsLocked(t)
	keys := pipeComposer(t)
	peer := newFakeGatewayPeer(t, func(p *fakeGatewayPeer, method string, params map[string]any) (any, error) {
		return map[string]any{}, nil
	})
	view := newGatewayChatView(peer, map[string]any{"stored_session_id": "sid"}, false, nil, "")
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := view.Run(context.Background(), "", false)
		done <- struct {
			code int
			err  error
		}{code, err}
	}()
	time.Sleep(200 * time.Millisecond)
	keys("/quit\r")
	select {
	case r := <-done:
		if r.err != nil || r.code != 0 {
			t.Fatalf("code %d err %v", r.code, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the composer never read /quit")
	}
}
