package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

type relayLLMFakeHandle struct {
	name string
	seq  int
}

type relayLLMFakePop struct {
	handle *relayLLMFakeHandle
	output any
}

type relayLLMFakeRequestIntercept struct {
	name     string
	priority int
	fn       func(name string, request *tools.RelayLLMRequest, annotated any) (*tools.RelayLLMRequest, any)
}

type relayLLMFakeStreamContext struct{ responseCodec any }

type relayLLMFakeNext func(ctx context.Context, request *tools.RelayLLMRequest) (*tools.RelayAsyncIterator, error)

type relayLLMFakeStreamIntercept struct {
	name     string
	priority int
	fn       func(ctx context.Context, name string, request *tools.RelayLLMRequest, sctx relayLLMFakeStreamContext, next relayLLMFakeNext) (*tools.RelayAsyncIterator, error)
}

// In process stand in for the native binding with scopes, intercept chains, trace headers and codecs
type relayLLMFake struct {
	mu                sync.Mutex
	seq               int
	stack             []*relayLLMFakeHandle
	pops              []relayLLMFakePop
	requestIntercepts []relayLLMFakeRequestIntercept
	streamIntercepts  []relayLLMFakeStreamIntercept
	relay             *tools.NemoRelay
}

func newRelayLLMFake() *relayLLMFake {
	f := &relayLLMFake{}
	f.relay = &tools.NemoRelay{
		Scope:         &tools.NemoRelayScope{Push: f.push, Pop: f.pop, GetHandle: func(context.Context) (any, error) { return f.top(), nil }},
		ScopeType:     tools.NemoRelayScopeTypes{Agent: "agent", Function: "function"},
		GetScopeStack: f.scopeStack,
		Subscribers:   &tools.NemoRelaySubscribers{FlushAsync: func(context.Context) (any, error) { return nil, nil }},
		LLM:           &tools.NemoRelayLLM{Execute: f.execute, StreamExecute: f.streamExecute},
		LLMRequest: func(headers, content map[string]any) (any, error) {
			return &tools.RelayLLMRequest{Headers: headers, Content: content}, nil
		},
		Codecs: &tools.NemoRelayCodecs{
			OpenAIChatCodec:        func() (*tools.NemoRelayCodec, error) { return relayLLMFakeCodec(false), nil },
			OpenAIResponsesCodec:   func() (*tools.NemoRelayCodec, error) { return relayLLMFakeCodec(false), nil },
			AnthropicMessagesCodec: func() (*tools.NemoRelayCodec, error) { return relayLLMFakeCodec(true), nil },
		},
	}
	return f
}

// Typed codec double, the anthropic one dropping cache_control like a typed model that cannot carry it
func relayLLMFakeCodec(lossy bool) *tools.NemoRelayCodec {
	return &tools.NemoRelayCodec{
		Decode: func(_ context.Context, request any) (any, error) {
			r := request.(*tools.RelayLLMRequest)
			return relayLLMFakeStrip(r.Content, lossy), nil
		},
		Encode: func(_ context.Context, decoded, request any) (any, error) {
			return &tools.RelayLLMRequest{Headers: request.(*tools.RelayLLMRequest).Headers, Content: decoded}, nil
		},
	}
}

func relayLLMFakeStrip(v any, lossy bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			if lossy && k == "cache_control" {
				continue
			}
			out[k] = relayLLMFakeStrip(item, lossy)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = relayLLMFakeStrip(item, lossy)
		}
		return out
	}
	return v
}

func (f *relayLLMFake) push(_ context.Context, name string, _ any, _ map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	h := &relayLLMFakeHandle{name: name, seq: f.seq}
	f.stack = append(f.stack, h)
	return h, nil
}

func (f *relayLLMFake) pop(_ context.Context, handle any, kwargs map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, _ := handle.(*relayLLMFakeHandle)
	for i := len(f.stack) - 1; i >= 0; i-- {
		if f.stack[i] == h {
			f.stack = append(f.stack[:i], f.stack[i+1:]...)
			break
		}
	}
	f.pops = append(f.pops, relayLLMFakePop{handle: h, output: kwargs["output"]})
	return nil, nil
}

func (f *relayLLMFake) top() any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stack) == 0 {
		return nil
	}
	return f.stack[len(f.stack)-1]
}

func (f *relayLLMFake) scopeStack(context.Context) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]any, len(f.stack))
	for i, h := range f.stack {
		out[i] = h
	}
	return out, nil
}

func (f *relayLLMFake) popOutputs() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]any, len(f.pops))
	for i, p := range f.pops {
		out[i] = p.output
	}
	return out
}

func (f *relayLLMFake) registerLLMRequest(name string, priority int, fn func(string, *tools.RelayLLMRequest, any) (*tools.RelayLLMRequest, any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requestIntercepts = append(f.requestIntercepts, relayLLMFakeRequestIntercept{name, priority, fn})
}

func (f *relayLLMFake) registerLLMStreamExecution(name string, priority int, fn func(context.Context, string, *tools.RelayLLMRequest, relayLLMFakeStreamContext, relayLLMFakeNext) (*tools.RelayAsyncIterator, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamIntercepts = append(f.streamIntercepts, relayLLMFakeStreamIntercept{name, priority, fn})
}

func (f *relayLLMFake) deregister(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req []relayLLMFakeRequestIntercept
	for _, ic := range f.requestIntercepts {
		if ic.name != name {
			req = append(req, ic)
		}
	}
	var str []relayLLMFakeStreamIntercept
	for _, ic := range f.streamIntercepts {
		if ic.name != name {
			str = append(str, ic)
		}
	}
	f.requestIntercepts, f.streamIntercepts = req, str
}

func relayLLMFakeTraceparent() string {
	trace, parent := make([]byte, 16), make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(parent)
	return "00-" + hex.EncodeToString(trace) + "-" + hex.EncodeToString(parent) + "-01"
}

// Runs request intercepts by priority and stamps the managed call trace header
func (f *relayLLMFake) prepare(operation string, request any) *tools.RelayLLMRequest {
	req := request.(*tools.RelayLLMRequest)
	f.mu.Lock()
	intercepts := append([]relayLLMFakeRequestIntercept(nil), f.requestIntercepts...)
	f.mu.Unlock()
	sort.SliceStable(intercepts, func(i, j int) bool { return intercepts[i].priority < intercepts[j].priority })
	var annotated any
	for _, ic := range intercepts {
		req, annotated = ic.fn(operation, req, annotated)
	}
	headers := map[string]any{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	headers["traceparent"] = relayLLMFakeTraceparent()
	return &tools.RelayLLMRequest{Headers: headers, Content: req.Content}
}

func (f *relayLLMFake) execute(_ context.Context, operation string, request any, callback tools.RelayProviderCallback, _ map[string]any) (any, error) {
	traced := f.prepare(operation, request)
	return tools.RelayAwaitable(func(ctx context.Context) (any, error) {
		v, err := callback(ctx, traced)
		if aw, ok := v.(tools.RelayAwaitable); ok && err == nil {
			return aw(ctx)
		}
		return v, err
	}), nil
}

// Lazy managed stream running stream intercepts around the provider and the finalizer at exhaustion
func (f *relayLLMFake) streamExecute(_ context.Context, operation string, request any, provider tools.RelayProviderStreamCallback, observe func(context.Context, any) error, finalizer func(context.Context) (any, error), kwargs map[string]any) (any, error) {
	traced := f.prepare(operation, request)
	f.mu.Lock()
	intercepts := append([]relayLLMFakeStreamIntercept(nil), f.streamIntercepts...)
	f.mu.Unlock()
	sort.SliceStable(intercepts, func(i, j int) bool { return intercepts[i].priority > intercepts[j].priority })
	chain := relayLLMFakeNext(func(ctx context.Context, r *tools.RelayLLMRequest) (*tools.RelayAsyncIterator, error) { return provider(ctx, r) })
	sctx := relayLLMFakeStreamContext{responseCodec: kwargs["response_codec"]}
	for _, ic := range intercepts {
		inner, ic := chain, ic
		chain = func(ctx context.Context, r *tools.RelayLLMRequest) (*tools.RelayAsyncIterator, error) {
			return ic.fn(ctx, operation, r, sctx, inner)
		}
	}
	var upstream *tools.RelayAsyncIterator
	done := false
	it := &tools.RelayAsyncIterator{
		Next: func(ctx context.Context) (any, error) {
			if done {
				return nil, io.EOF
			}
			if upstream == nil {
				up, err := chain(ctx, traced)
				if err != nil {
					done = true
					return nil, err
				}
				upstream = up
			}
			chunk, err := upstream.Next(ctx)
			if err == io.EOF {
				done = true
				if _, ferr := finalizer(ctx); ferr != nil {
					return nil, ferr
				}
				return nil, io.EOF
			}
			if err != nil {
				done = true
				return nil, err
			}
			if oerr := observe(ctx, chunk); oerr != nil {
				done = true
				return nil, oerr
			}
			return chunk, nil
		},
		Aclose: func(ctx context.Context) error {
			done = true
			if upstream != nil && upstream.Aclose != nil {
				return upstream.Aclose(ctx)
			}
			return nil
		},
	}
	return tools.RelayAwaitable(func(context.Context) (any, error) { return it, nil }), nil
}

// Async generator double driven on its own goroutine, yield reporting false once aclose threw GeneratorExit
func relayLLMTestGen(body func(ctx context.Context, yield func(any) bool) error) *tools.RelayAsyncIterator {
	type step struct {
		v    any
		err  error
		done bool
	}
	resume, out := make(chan bool), make(chan step)
	started, finished := false, false
	yield := func(v any) bool {
		out <- step{v: v}
		return <-resume
	}
	next := func(ctx context.Context) (any, error) {
		if finished {
			return nil, io.EOF
		}
		if !started {
			started = true
			go func() {
				err := body(ctx, yield)
				out <- step{err: err, done: true}
			}()
		} else {
			resume <- true
		}
		s := <-out
		if !s.done {
			return s.v, nil
		}
		finished = true
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	aclose := func(context.Context) error {
		if !started || finished {
			finished = true
			return nil
		}
		resume <- false
		for s := range out {
			if s.done {
				break
			}
			resume <- false
		}
		finished = true
		return nil
	}
	return &tools.RelayAsyncIterator{Next: next, Aclose: aclose}
}

// Binds a fake binding loader under a fresh profile home and resets the runtime around the test
func relayLLMInstallFake(t *testing.T, home string) *relayLLMFake {
	t.Helper()
	t.Setenv("ICEBATH_HOME", filepath.Join(t.TempDir(), home))
	f := newRelayLLMFake()
	tools.RegisterNemoRelayBinding(func() (*tools.NemoRelay, error) { return f.relay, nil })
	tools.ResetRelayRuntimeForTests()
	t.Cleanup(func() {
		tools.ResetRelayRuntimeForTests()
		tools.RegisterNemoRelayBinding(nil)
	})
	return f
}

// Conversation and turn over the fake binding, retaining managed execution when managed is set
func relayLLMOpenTurn(t *testing.T, f *relayLLMFake, sessionID, turnID, taskID string, managed bool) (context.Context, *tools.RelayTurnContext) {
	t.Helper()
	ctx := pyctx.With(context.Background(), pyctx.New())
	lease := tools.RelayCoordinator.AcquireConversation(ctx, tools.RelayConversation{ProfileKey: tools.CurrentRelayProfileKey(ctx), SessionID: sessionID, Platform: "cli"})
	turn, err := tools.RelayCoordinator.BeginTurn(ctx, lease, turnID, taskID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if managed {
		if err := lease.Host.RetainManagedExecution("test.relay_llm"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if managed {
			lease.Host.ReleaseManagedExecution("test.relay_llm")
		}
		tools.RelayCoordinator.EndTurn(ctx, turn, "success")
		tools.RelayCoordinator.ReleaseConversation(lease)
	})
	return ctx, turn
}

// The relay_turn fixture, session-1 with managed execution retained
func relayLLMTurn(t *testing.T) (*relayLLMFake, context.Context, *tools.RelayTurnContext) {
	t.Helper()
	f := relayLLMInstallFake(t, "profile")
	ctx, turn := relayLLMOpenTurn(t, f, "session-1", "turn-1", "task-1", true)
	return f, ctx, turn
}

func relayLLMLogicalCalls(turn *tools.RelayTurnContext) map[string]any {
	turn.LogicalLLMMu.Lock()
	defer turn.LogicalLLMMu.Unlock()
	out := map[string]any{}
	for k, v := range turn.LogicalLLMCalls.All() {
		out[k] = v
	}
	return out
}

// Drains a managed stream into a list, failing the test on any error other than exhaustion
func relayLLMDrain(t *testing.T, ctx context.Context, s interface {
	Next(context.Context) (any, error)
}) []any {
	t.Helper()
	var out []any
	for {
		chunk, err := s.Next(ctx)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		out = append(out, chunk)
	}
}

// Iterator over provider chunks with a counted close that may fail
type relayLLMRawStream struct {
	mu         sync.Mutex
	chunks     []any
	closeCalls int
	closeErr   error
	onNext     func(i int) error
	i          int
}

func (r *relayLLMRawStream) Next(context.Context) (any, error) {
	r.mu.Lock()
	i := r.i
	r.i++
	r.mu.Unlock()
	if r.onNext != nil {
		if err := r.onNext(i); err != nil {
			return nil, err
		}
	}
	if i >= len(r.chunks) {
		return nil, io.EOF
	}
	return r.chunks[i], nil
}

func (r *relayLLMRawStream) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCalls++
	return r.closeErr
}
