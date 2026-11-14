package acp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type rpcCall struct {
	method string
	params map[string]any
}

type fakeGateway struct {
	mu     sync.Mutex
	events *gateway.EventQueue
	answer func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error)
	calls  []rpcCall
	closed bool
	done   bool
}

func newFakeGateway(answer func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error)) *fakeGateway {
	return &fakeGateway{events: gateway.NewEventQueue(0), answer: answer}
}

func (g *fakeGateway) RPC(ctx context.Context, method string, params map[string]any, timeout time.Duration) (*pyjson.Object, error) {
	g.mu.Lock()
	g.calls = append(g.calls, rpcCall{method, params})
	g.mu.Unlock()
	return g.answer(ctx, method, params)
}

func (g *fakeGateway) Events() *gateway.EventQueue { return g.events }

func (g *fakeGateway) ReaderDone() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

func (g *fakeGateway) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	return nil
}

func (g *fakeGateway) called(method string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []map[string]any
	for _, call := range g.calls {
		if call.method == method {
			out = append(out, call.params)
		}
	}
	return out
}

func (g *fakeGateway) put(frame any) { g.events.PutNowait(frame) }

type editorFake struct {
	captureConn
	onUpdate func(sessionID string, update pyacp.SessionUpdate) error
	request  func(method string, params any) (any, error)
}

func (e *editorFake) SessionUpdate(ctx context.Context, sessionID string, update pyacp.SessionUpdate, meta *pyjson.Object) error {
	e.captureConn.SessionUpdate(ctx, sessionID, update, meta)
	if e.onUpdate != nil {
		return e.onUpdate(sessionID, update)
	}
	return nil
}

func (e *editorFake) SendRequest(ctx context.Context, method string, params any) (any, error) {
	if e.request != nil {
		return e.request(method, params)
	}
	return nil, nil
}

func jsonObj(t testing.TB, text string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(text)
	if err != nil {
		t.Fatalf("bad json %s: %v", text, err)
	}
	return v.(*pyjson.Object)
}

func eventFrame(t testing.TB, kind, payload, session, admission string, seq int) *pyjson.Object {
	t.Helper()
	params := pyjson.NewObject()
	params.Set("type", kind)
	params.Set("session_id", session)
	params.Set("payload", jsonObj(t, payload))
	if admission == "" {
		params.Set("admission_id", nil)
	} else {
		params.Set("admission_id", admission)
	}
	if seq != 0 {
		params.Set("replay_epoch", "e")
		params.Set("seq", int64(seq))
	}
	return pyjson.ObjectOf("method", "event", "params", params)
}

func shownText(e *editorFake, session string) string {
	var b strings.Builder
	for _, u := range e.seen() {
		if u.sessionID != session {
			continue
		}
		var content pyacp.ContentBlock
		switch update := u.update.(type) {
		case *pyacp.AgentMessageChunk:
			content = update.Content
		case *pyacp.AgentThoughtChunk:
			content = update.Content
		case *pyacp.UserMessageChunk:
			content = update.Content
		}
		if text, ok := content.(*pyacp.TextContentBlock); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func onLoop[T any](a *GatewayACPAgent, fn func() T) T {
	a.loop.enter()
	defer a.loop.leave()
	return fn()
}

func until(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type asked struct {
	done chan struct{}
	resp *pyacp.PromptResponse
	err  error
}

func (q *asked) wait(t testing.TB, timeout time.Duration) (*pyacp.PromptResponse, error) {
	t.Helper()
	select {
	case <-q.done:
		return q.resp, q.err
	case <-time.After(timeout):
		t.Fatalf("prompt did not finish in %s", timeout)
		return nil, nil
	}
}

func (q *asked) finished() bool {
	select {
	case <-q.done:
		return true
	default:
		return false
	}
}

type gatewayRig struct {
	t      testing.TB
	agent  *GatewayACPAgent
	link   *fakeGateway
	editor *editorFake
	ctx    context.Context
	cancel context.CancelFunc
	asks   []*asked
}

func newGatewayAgent(t testing.TB) *GatewayACPAgent {
	t.Helper()
	if os := testingHome(t); os == "" {
		t.Fatal("no home")
	}
	return NewGatewayACPAgent()
}

func testingHome(t testing.TB) string {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	return dir
}

func newRig(t testing.TB, answer func(r *gatewayRig, ctx context.Context, method string, params map[string]any) (*pyjson.Object, error)) *gatewayRig {
	t.Helper()
	r := &gatewayRig{t: t, agent: newGatewayAgent(t), editor: &editorFake{}}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.link = newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*pyjson.Object, error) {
		return answer(r, ctx, method, params)
	})
	a := r.agent
	a.loop.enter()
	for _, sid := range []string{"s", "t"} {
		a.snapshots.Set(sid, jsonObj(t, `{"replay_epoch": "e", "last_sequence": 0}`))
	}
	a.gateway, a.connection, a.conn = r.link, r.link, r.editor
	a.eventTask = a.loop.createTask(a.ctx, func(ctx context.Context) (any, error) { return nil, a.events(ctx) }, nil)
	a.loop.leave()
	t.Cleanup(func() {
		r.cancel()
		a.loop.enter()
		task := a.eventTask
		a.loop.leave()
		if task != nil {
			task.Cancel()
			<-task.done
		}
		for _, q := range r.asks {
			<-q.done
		}
	})
	return r
}

func (r *gatewayRig) ask(session string) *asked {
	q := &asked{done: make(chan struct{})}
	r.asks = append(r.asks, q)
	go func() {
		defer close(q.done)
		q.resp, q.err = r.agent.Prompt(r.ctx, textPrompt(session, "q"))
	}()
	return q
}

func (r *gatewayRig) admission(session string) string {
	return onLoop(r.agent, func() string { return r.agent.admissions[session] })
}

func (r *gatewayRig) resumed(session string) bool {
	return onLoop(r.agent, func() bool {
		snapshot, _ := r.agent.snapshots.Get(session)
		return dget(snapshot, "resumed") == true
	})
}

func resumedSnapshot(t testing.TB, pending string) *pyjson.Object {
	return jsonObj(t, `{"replay_epoch": "e2", "last_sequence": 0, "pending": [`+pending+`], "prompts": [], "resumed": true}`)
}
