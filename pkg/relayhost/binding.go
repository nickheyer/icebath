package relayhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Python context the binding last left for a Go context, copied by pyctx the way contextvars copies it
var relayContextVar = pyctx.NewVar("icebath.relayhost.context", nil)

type pyContext struct {
	client *Client
	id     int64
}

type originKey struct{}

// Native object the child holds for the parent, its uuid read when it has one
type Object struct {
	id   int64
	uuid any
}

// Native uuid of a scope, tool or LLM handle, nil for other objects
func (o *Object) UUID() any { return o.uuid }

// Pending op of the parent, the context its child callbacks run under and the callback errors they raised
type clientOp struct {
	ctx  context.Context
	mu   sync.Mutex
	errs map[int64]error
	next int64
}

// Parent end of a relay host child, the production NeMo Relay binding
type Client struct {
	python    string
	cmd       *exec.Cmd
	stdin     io.Closer
	c         *conn
	mu        sync.Mutex
	nextCB    int64
	callbacks map[int64]any
	ops       map[int64]*clientOp
	codecs    map[weak.Pointer[tools.NemoRelayCodec]]*Object
	releases  chan int64
	relay     *tools.NemoRelay
}

// Entry name the parent re-executes, refusing a binary whose main never dispatched it
func childArgv(python string) ([]string, error) {
	if !entryDispatched.Load() {
		return nil, errors.New("the relay host would re-execute a binary that never called relayhost.DispatchRelayHostEntry, call it first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, Entry, "--python", python}, nil
}

// Starts a child hosting python and imports nemo_relay there
func Start(ctx context.Context, python string) (*Client, error) {
	argv, err := childArgv(python)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	cl := &Client{python: python, cmd: cmd, stdin: stdin, callbacks: map[int64]any{}, ops: map[int64]*clientOp{},
		codecs: map[weak.Pointer[tools.NemoRelayCodec]]*Object{}, releases: make(chan int64, 4096)}
	cl.c = newConn(stdout, stdin, false, cl.serve)
	go cl.c.readLoop()
	go cl.releaseLoop()
	go func() {
		<-cl.c.done
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	hello, err := cl.request(ctx, "hello", map[string]any{})
	if err != nil {
		cl.Close()
		return nil, err
	}
	info, _ := hello.(map[string]any)
	cl.relay = cl.binding(info)
	return cl, nil
}

// Whether the child still serves requests
func (c *Client) Alive() bool { return c.c.err() == nil }

// Closes the channel, ending the child once it finalizes its interpreter, killing it past the finalize deadline
func (c *Client) Close() {
	_ = c.stdin.Close()
	time.AfterFunc(finalizeDeadline+5*time.Second, func() { _ = c.cmd.Process.Kill() })
}

// Sends released child ids in batches from a goroutine of its own
func (c *Client) releaseLoop() {
	for {
		select {
		case id := <-c.releases:
			ids := []any{id}
			for more := true; more && len(ids) < 512; {
				select {
				case next := <-c.releases:
					ids = append(ids, next)
				default:
					more = false
				}
			}
			_, _, _ = c.c.open("release", 0, map[string]any{"ids": ids})
		case <-c.c.done:
			return
		}
	}
}

func (c *Client) release(id int64) {
	select {
	case c.releases <- id:
	default:
		go func() { c.releases <- id }()
	}
}

// Sends one op and waits for its reply, cancelling the child's await when ctx ends first
func (c *Client) request(ctx context.Context, op string, args map[string]any) (any, error) {
	state := &clientOp{ctx: ctx, errs: map[int64]error{}}
	origin, _ := ctx.Value(originKey{}).(int64)
	id, ch, err := c.c.openWith(op, origin, args, func(id int64) {
		c.mu.Lock()
		c.ops[id] = state
		c.mu.Unlock()
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		c.mu.Lock()
		delete(c.ops, id)
		c.mu.Unlock()
	}()
	done := ctx.Done()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return nil, c.c.err()
			}
			return c.result(state, m)
		case <-done:
			done = nil
			_, _, _ = c.c.open("cancel", 0, map[string]any{"id": id})
		}
	}
}

func (c *Client) result(state *clientOp, m *message) (any, error) {
	if m.Error != nil {
		state.mu.Lock()
		goErr := state.errs[m.Error.GoErr]
		state.mu.Unlock()
		switch {
		case m.Error.GoErr != 0 && goErr != nil:
			return nil, goErr
		case m.Error.Class == "CancelledError" && state.ctx.Err() != nil:
			return nil, state.ctx.Err()
		}
		return nil, &tools.RelayError{Class: m.Error.Class, Message: m.Error.Message}
	}
	raw, err := decodeJSON(m.Result)
	if err != nil {
		return nil, err
	}
	return liftValue(raw, c.liftTag)
}

// Go value of a tagged node the child sent
func (c *Client) liftTag(tag string, body any) (any, error) {
	parts, _ := body.([]any)
	switch tag {
	case "$r":
		if len(parts) != 3 {
			return nil, errors.New("ValueError: malformed native object reference")
		}
		o := &Object{id: wireInt(parts[0]), uuid: parts[1]}
		runtime.AddCleanup(o, c.release, o.id)
		if parts[2] == true {
			return tools.RelayAwaitable(func(ctx context.Context) (any, error) {
				return c.await(ctx, map[string]any{"ref": o.id}, false, o)
			}), nil
		}
		return o, nil
	case "$q":
		lifted, err := liftValue(body, c.liftTag)
		if err != nil {
			return nil, err
		}
		pair, _ := lifted.([]any)
		if len(pair) != 2 {
			return nil, errors.New("ValueError: malformed LLMRequest")
		}
		headers, _ := pair[0].(map[string]any)
		return &tools.RelayLLMRequest{Headers: headers, Content: pair[1]}, nil
	case "$t":
		lifted, err := liftValue(body, c.liftTag)
		if err != nil {
			return nil, err
		}
		return &tools.RelayToolExecutionResult{Result: lifted}, nil
	case "$e":
		fields, _ := body.(map[string]any)
		lifted, err := liftMap(fields, c.liftTag)
		if err != nil {
			return nil, err
		}
		return &tools.RelayEventView{Kind: lifted["kind"], Name: lifted["name"], Category: lifted["category"],
			ScopeCategory: lifted["scope_category"], CategoryProfile: lifted["category_profile"], Metadata: lifted["metadata"],
			Data: lifted["data"]}, nil
	}
	return nil, fmt.Errorf("ValueError: unknown wire tag %s", tag)
}

// Wire tree of the arguments of an op, Go callbacks and iterators registered for the child to call back
func (c *Client) lower(v any) (any, error) {
	return lowerValue(v, 0, c.lowerSpecial)
}

func (c *Client) lowerSpecial(v any, depth int) (any, bool, error) {
	switch x := v.(type) {
	case *Object:
		return map[string]any{"$r": x.id}, true, nil
	case *tools.RelayLLMRequest:
		headers, err := lowerValue(x.Headers, depth+1, c.lowerSpecial)
		if err != nil {
			return nil, true, err
		}
		content, err := lowerValue(x.Content, depth+1, c.lowerSpecial)
		return map[string]any{"$q": []any{headers, content}}, true, err
	case *tools.RelayToolExecutionResult:
		result, err := lowerValue(x.Result, depth+1, c.lowerSpecial)
		return map[string]any{"$t": result}, true, err
	case *tools.NemoRelayCodec:
		c.mu.Lock()
		o := c.codecs[weak.Make(x)]
		c.mu.Unlock()
		if o == nil {
			return nil, true, &wireError{Class: "TypeError", Message: "codec was not created by this NeMo Relay binding"}
		}
		return map[string]any{"$r": o.id}, true, nil
	case *pyjson.Object:
		pairs := make([]any, 0, len(x.Keys))
		for _, k := range x.Keys {
			item, err := lowerValue(x.Vals[k], depth+1, c.lowerSpecial)
			if err != nil {
				return nil, true, err
			}
			pairs = append(pairs, []any{k, item})
		}
		return map[string]any{"$o": pairs}, true, nil
	case time.Time:
		return map[string]any{"$m": x.Format(time.RFC3339Nano)}, true, nil
	case *tools.RelayAsyncIterator:
		return map[string]any{"$x": []any{c.register(x), x.Aclose != nil}}, true, nil
	case tools.RelayProviderCallback:
		return c.callbackNode(x, "call"), true, nil
	case func(context.Context, any) (any, error):
		return c.callbackNode(tools.RelayProviderCallback(x), "call"), true, nil
	case tools.RelayProviderStreamCallback:
		return c.callbackNode(x, "stream"), true, nil
	case func(context.Context, any) (*tools.RelayAsyncIterator, error):
		return c.callbackNode(tools.RelayProviderStreamCallback(x), "stream"), true, nil
	case func(context.Context, any) error:
		return c.callbackNode(x, "observe"), true, nil
	case func(context.Context) (any, error):
		return c.callbackNode(x, "final"), true, nil
	case func(*tools.RelayEventView):
		return c.callbackNode(x, "event"), true, nil
	}
	return nil, false, nil
}

func (c *Client) register(fn any) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextCB++
	c.callbacks[c.nextCB] = fn
	return c.nextCB
}

func (c *Client) callbackNode(fn any, kind string) map[string]any {
	return map[string]any{"$c": []any{c.register(fn), kind}}
}

// Serves a child request on its own goroutine under the context of the parent op it came from
func (c *Client) serve(m *message) {
	c.mu.Lock()
	state := c.ops[m.Origin]
	c.mu.Unlock()
	ctx := context.Background()
	if state != nil {
		ctx = state.ctx
	}
	ctx = context.WithValue(ctx, originKey{}, m.ID)
	go func() {
		result, err := c.handle(ctx, m)
		if m.Op == "release" {
			return
		}
		if err != nil {
			_ = c.c.reply(m.ID, nil, c.callbackFailure(state, err))
			return
		}
		lowered, err := c.lower(result)
		if err != nil {
			_ = c.c.reply(m.ID, nil, c.callbackFailure(state, err))
			return
		}
		_ = c.c.reply(m.ID, lowered, nil)
	}()
}

// Wire exception of a Go callback error, named like its Go type and remembered by the op it unwinds
func (c *Client) callbackFailure(state *clientOp, err error) *wireError {
	var we *wireError
	if errors.As(err, &we) && we.Class == "StopAsyncIteration" {
		return we
	}
	kind := reflect.TypeOf(err)
	for kind.Kind() == reflect.Pointer && kind.Name() == "" {
		kind = kind.Elem()
	}
	module := kind.PkgPath()
	if i := strings.LastIndex(module, "/"); i >= 0 {
		module = module[i+1:]
	}
	if module == "" {
		module = "builtins"
	}
	out := &wireError{Class: kind.Name(), Module: module, Message: err.Error()}
	if state != nil {
		state.mu.Lock()
		state.next++
		out.GoErr = state.next
		state.errs[state.next] = err
		state.mu.Unlock()
	}
	return out
}

func (c *Client) handle(ctx context.Context, m *message) (any, error) {
	raw, err := decodeJSON(m.Args)
	if err != nil {
		return nil, err
	}
	lifted, err := liftValue(raw, c.liftTag)
	if err != nil {
		return nil, err
	}
	args, _ := lifted.(map[string]any)
	id := wireInt(args["id"])
	if m.Op == "release" {
		ids, _ := args["ids"].([]any)
		c.mu.Lock()
		for _, released := range ids {
			delete(c.callbacks, wireInt(released))
		}
		c.mu.Unlock()
		return nil, nil
	}
	c.mu.Lock()
	fn := c.callbacks[id]
	c.mu.Unlock()
	if fn == nil {
		return nil, &wireError{Class: "ReferenceError", Message: fmt.Sprintf("parent callback %d was released", id)}
	}
	positional, _ := args["args"].([]any)
	arg := func(i int) any {
		if i < len(positional) {
			return positional[i]
		}
		return nil
	}
	switch m.Op {
	case "iter_next":
		it := fn.(*tools.RelayAsyncIterator)
		v, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil, &wireError{Class: "StopAsyncIteration", Message: ""}
		}
		return v, err
	case "iter_close":
		it := fn.(*tools.RelayAsyncIterator)
		if it.Aclose == nil {
			return nil, &wireError{Class: "AttributeError", Message: "'ParentAsyncIterator' object has no attribute 'aclose'"}
		}
		return nil, it.Aclose(ctx)
	}
	switch f := fn.(type) {
	case tools.RelayProviderCallback:
		v, err := f(ctx, arg(0))
		if aw, ok := v.(tools.RelayAwaitable); ok && err == nil {
			return aw(ctx)
		}
		return v, err
	case tools.RelayProviderStreamCallback:
		return f(ctx, arg(0))
	case func(context.Context, any) error:
		return nil, f(ctx, arg(0))
	case func(context.Context) (any, error):
		return f(ctx)
	case func(*tools.RelayEventView):
		ev, ok := arg(0).(*tools.RelayEventView)
		if !ok {
			return nil, &wireError{Class: "TypeError", Message: fmt.Sprintf("subscriber %d received %T instead of a Relay event", id, arg(0))}
		}
		f(ev)
		return nil, nil
	}
	return nil, &wireError{Class: "TypeError", Message: fmt.Sprintf("parent callback %d of type %T is not callable", id, fn)}
}

// Python context id the binding left for ctx, zero when ctx carries none of this child
func (c *Client) contextID(ctx context.Context) int64 {
	if p, ok := relayContextVar.Get(ctx).(*pyContext); ok && p != nil && p.client == c {
		return p.id
	}
	return 0
}

func (c *Client) setContext(ctx context.Context, id int64) {
	p := &pyContext{client: c, id: id}
	runtime.AddCleanup(p, c.release, id)
	relayContextVar.Set(ctx, p)
}

// Environment and working directory plugin discovery reads in the child
func processEnvironment() (map[string]any, string) {
	env := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	cwd, _ := os.Getwd()
	return env, cwd
}

// Calls fn synchronously in ctx's Python context and keeps the context it leaves for ctx
func (c *Client) call(ctx context.Context, fn any, args []any, kwargs map[string]any, withEnv bool) (any, error) {
	payload, err := c.payload(ctx, fn, args, kwargs, withEnv)
	if err != nil {
		return nil, err
	}
	out, err := c.request(ctx, "call", payload)
	if err != nil {
		return nil, err
	}
	reply, _ := out.(map[string]any)
	if id := wireInt(reply["ctx"]); id != 0 {
		c.setContext(ctx, id)
	}
	return reply["value"], nil
}

func (c *Client) payload(ctx context.Context, fn any, args []any, kwargs map[string]any, withEnv bool) (map[string]any, error) {
	if args == nil {
		args = []any{}
	}
	if kwargs == nil {
		kwargs = map[string]any{}
	}
	pos, err := c.lower(args)
	if err != nil {
		return nil, err
	}
	kw, err := c.lower(kwargs)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"ctx": c.contextID(ctx), "fn": fn, "args": pos, "kwargs": kw}
	if withEnv {
		payload["env"], payload["cwd"] = processEnvironment()
	}
	return payload, nil
}

// Awaits a call on a fresh loop in a copy of ctx's Python context, keeping the call's objects alive
func (c *Client) await(ctx context.Context, fn any, invoke bool, keep ...any) (any, error) {
	v, _, err := c.awaitOn(ctx, fn, invoke, nil, nil, 0, false)
	runtime.KeepAlive(keep)
	return v, err
}

func (c *Client) awaitOn(ctx context.Context, fn any, invoke bool, args []any, kwargs map[string]any, loop int64, withEnv bool) (any, int64, error) {
	payload, err := c.payload(ctx, fn, args, kwargs, withEnv)
	if err != nil {
		return nil, 0, err
	}
	payload["invoke"], payload["loop"] = invoke, loop
	out, err := c.request(ctx, "run", payload)
	if err != nil {
		return nil, 0, err
	}
	reply, _ := out.(map[string]any)
	return reply["value"], wireInt(reply["loop"]), nil
}

// Deferred managed call run on its own loop when awaited, the way the hosted coroutine is awaited
func (c *Client) deferred(path string, args []any, kwargs map[string]any, withEnv bool) tools.RelayAwaitable {
	return func(ctx context.Context) (any, error) {
		v, _, err := c.awaitOn(ctx, path, true, args, kwargs, 0, withEnv)
		return v, err
	}
}

// Attribute of a held native object
func (c *Client) attr(ctx context.Context, o *Object, name string) (any, error) {
	v, err := c.request(ctx, "attr", map[string]any{"ref": o.id, "name": name})
	runtime.KeepAlive(o)
	return v, err
}

func optionalPath(path string) any {
	if path == "" {
		return nil
	}
	return path
}

// Binding surface whose members exist exactly where nemo_relay has the attribute
func (c *Client) binding(info map[string]any) *tools.NemoRelay {
	has := map[string]bool{}
	paths, _ := info["paths"].([]any)
	for _, p := range paths {
		if s, ok := p.(string); ok {
			has[s] = true
		}
	}
	callFn := func(path string, withEnv bool) func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		return func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
			return c.call(ctx, path, args, kwargs, withEnv)
		}
	}
	r := &tools.NemoRelay{}
	if has["scope"] {
		s := &tools.NemoRelayScope{}
		if has["scope.push"] {
			push := callFn("scope.push", false)
			s.Push = func(ctx context.Context, name string, scopeType any, kwargs map[string]any) (any, error) {
				return push(ctx, []any{name, scopeType}, kwargs)
			}
		}
		if has["scope.pop"] {
			pop := callFn("scope.pop", false)
			s.Pop = func(ctx context.Context, handle any, kwargs map[string]any) (any, error) {
				return pop(ctx, []any{handle}, kwargs)
			}
			params, popErr := popParameters(info)
			s.PopParameters = func() ([]tools.RelayParameter, error) { return params, popErr }
		}
		if has["scope.get_handle"] {
			get := callFn("scope.get_handle", false)
			s.GetHandle = func(ctx context.Context) (any, error) { return get(ctx, nil, nil) }
		}
		if has["scope.event"] {
			event := callFn("scope.event", false)
			s.Event = func(ctx context.Context, name string, kwargs map[string]any) (any, error) {
				return event(ctx, []any{name}, kwargs)
			}
		}
		r.Scope = s
	}
	if has["ScopeType"] {
		r.ScopeType = tools.NemoRelayScopeTypes{Agent: info["agent"], Function: info["function"]}
	}
	if has["get_scope_stack"] {
		get := callFn("get_scope_stack", false)
		r.GetScopeStack = func(ctx context.Context) (any, error) { return get(ctx, nil, nil) }
	}
	if has["plugin"] {
		p := &tools.NemoRelayPlugin{}
		if has["plugin.initialize"] {
			p.Initialize = func(_ context.Context, config map[string]any, toml string) (any, error) {
				return tools.RelayAwaitable(func(ctx context.Context) (any, error) {
					v, _, err := c.awaitOn(ctx, "plugin.initialize", true, []any{config},
						map[string]any{"additional_plugins_toml": optionalPath(toml)}, 0, true)
					if err != nil {
						return nil, err
					}
					return c.activation(v), nil
				}), nil
			}
		}
		if has["plugin.validate"] {
			validate := callFn("plugin.validate", true)
			p.Validate = func(ctx context.Context, config map[string]any, toml string) (map[string]any, error) {
				v, err := validate(ctx, []any{config}, map[string]any{"additional_plugins_toml": optionalPath(toml)})
				m, _ := v.(map[string]any)
				return m, err
			}
		}
		if has["plugin.validate_exact"] {
			exact := callFn("plugin.validate_exact", true)
			p.ValidateExact = func(ctx context.Context, config map[string]any) (map[string]any, error) {
				v, err := exact(ctx, []any{config}, nil)
				m, _ := v.(map[string]any)
				return m, err
			}
		}
		r.Plugin = p
	}
	if has["subscribers"] {
		s := &tools.NemoRelaySubscribers{}
		if has["subscribers.register"] {
			register := callFn("subscribers.register", false)
			s.Register = func(name string, callback func(event *tools.RelayEventView)) error {
				_, err := register(context.Background(), []any{name, callback}, nil)
				return err
			}
		}
		if has["subscribers.deregister"] {
			deregister := callFn("subscribers.deregister", false)
			s.Deregister = func(name string) error {
				_, err := deregister(context.Background(), []any{name}, nil)
				return err
			}
		}
		if has["subscribers.flush"] {
			flush := callFn("subscribers.flush", false)
			s.Flush = func() error {
				_, err := flush(context.Background(), nil, nil)
				return err
			}
		}
		if has["subscribers.flush_async"] {
			s.FlushAsync = func(context.Context) (any, error) {
				return c.deferred("subscribers.flush_async", nil, nil, false), nil
			}
		}
		r.Subscribers = s
	}
	if has["tools"] {
		t := &tools.NemoRelayTools{}
		if has["tools.request_intercepts"] {
			intercepts := callFn("tools.request_intercepts", false)
			t.RequestIntercepts = func(ctx context.Context, toolName string, args map[string]any) (any, error) {
				return intercepts(ctx, []any{toolName, args}, nil)
			}
		}
		if has["tools.execute"] {
			t.Execute = func(_ context.Context, toolName string, args any, callback tools.RelayProviderCallback, kwargs map[string]any) (any, error) {
				return c.deferred("tools.execute", []any{toolName, args, callback}, kwargs, false), nil
			}
		}
		if has["tools.call"] {
			call := callFn("tools.call", false)
			t.Call = func(ctx context.Context, name string, args map[string]any, kwargs map[string]any) (any, error) {
				return call(ctx, []any{name, args}, kwargs)
			}
		}
		if has["tools.call_end"] {
			end := callFn("tools.call_end", false)
			t.CallEnd = func(ctx context.Context, handle, result any, kwargs map[string]any) error {
				_, err := end(ctx, []any{handle, result}, kwargs)
				return err
			}
		}
		r.Tools = t
	}
	if has["llm"] {
		l := &tools.NemoRelayLLM{}
		if has["llm.execute"] {
			l.Execute = func(_ context.Context, operation string, request any, callback tools.RelayProviderCallback, kwargs map[string]any) (any, error) {
				return c.deferred("llm.execute", []any{operation, request, callback}, kwargs, false), nil
			}
		}
		if has["llm.stream_execute"] {
			l.StreamExecute = func(_ context.Context, operation string, request any, provider tools.RelayProviderStreamCallback, observe func(ctx context.Context, chunk any) error, finalizer func(ctx context.Context) (any, error), kwargs map[string]any) (any, error) {
				return tools.RelayAwaitable(func(ctx context.Context) (any, error) {
					v, loop, err := c.awaitOn(ctx, "llm.stream_execute", true, []any{operation, request, provider, observe, finalizer}, kwargs, -1, false)
					if err != nil {
						return nil, err
					}
					return c.stream(v, loop), nil
				}), nil
			}
		}
		if has["llm.call"] {
			call := callFn("llm.call", false)
			l.Call = func(ctx context.Context, name string, request any, kwargs map[string]any) (any, error) {
				return call(ctx, []any{name, request}, kwargs)
			}
		}
		if has["llm.call_end"] {
			end := callFn("llm.call_end", false)
			l.CallEnd = func(ctx context.Context, handle, response any, kwargs map[string]any) error {
				_, err := end(ctx, []any{handle, response}, kwargs)
				return err
			}
		}
		r.LLM = l
	}
	if has["LLMRequest"] {
		r.LLMRequest = func(headers, content map[string]any) (any, error) {
			return &tools.RelayLLMRequest{Headers: headers, Content: content}, nil
		}
	}
	if has["codecs"] {
		codec := func(path string) func() (*tools.NemoRelayCodec, error) {
			if !has[path] {
				return nil
			}
			return func() (*tools.NemoRelayCodec, error) { return c.codec(path) }
		}
		r.Codecs = &tools.NemoRelayCodecs{OpenAIChatCodec: codec("codecs.OpenAIChatCodec"),
			OpenAIResponsesCodec: codec("codecs.OpenAIResponsesCodec"), AnthropicMessagesCodec: codec("codecs.AnthropicMessagesCodec")}
	}
	if has["ToolExecutionResult"] {
		r.ToolExecutionResult = func(result any) (*tools.RelayToolExecutionResult, error) {
			return &tools.RelayToolExecutionResult{Result: result}, nil
		}
	}
	return r
}

func popParameters(info map[string]any) ([]tools.RelayParameter, error) {
	if failure, ok := info["pop_error"].(map[string]any); ok {
		class, _ := failure["class"].(string)
		msg, _ := failure["message"].(string)
		return nil, &tools.RelayError{Class: class, Message: msg}
	}
	raw, _ := info["pop_params"].([]any)
	out := make([]tools.RelayParameter, 0, len(raw))
	for _, item := range raw {
		pair, _ := item.([]any)
		if len(pair) != 2 {
			continue
		}
		name, _ := pair[0].(string)
		varKw, _ := pair[1].(bool)
		out = append(out, tools.RelayParameter{Name: name, VarKeyword: varKw})
	}
	return out, nil
}

// Plugin host activation whose report is read and whose close is awaited in the child
func (c *Client) activation(v any) any {
	o, ok := v.(*Object)
	if !ok {
		return v
	}
	return &tools.RelayActivation{
		Report: func() (any, error) { return c.attr(context.Background(), o, "report") },
		Close: func(context.Context) (any, error) {
			return tools.RelayAwaitable(func(ctx context.Context) (any, error) {
				return c.await(ctx, map[string]any{"ref": o.id, "method": "close"}, true, o)
			}), nil
		},
	}
}

// Managed stream iterator stepping the native stream on the private loop it was opened on
func (c *Client) stream(v any, loop int64) any {
	o, ok := v.(*Object)
	if !ok {
		if loop != 0 {
			c.release(loop)
		}
		return v
	}
	holder := &struct{ loop int64 }{loop}
	runtime.AddCleanup(holder, c.release, loop)
	step := func(ctx context.Context, method string) (any, error) {
		v, _, err := c.awaitOn(ctx, map[string]any{"ref": o.id, "method": method}, true, nil, nil, holder.loop, false)
		runtime.KeepAlive(o)
		runtime.KeepAlive(holder)
		return v, err
	}
	return &tools.RelayAsyncIterator{
		Next: func(ctx context.Context) (any, error) {
			v, err := step(ctx, "__anext__")
			var re *tools.RelayError
			if errors.As(err, &re) && re.Class == "StopAsyncIteration" {
				return nil, io.EOF
			}
			return v, err
		},
		Aclose: func(ctx context.Context) error {
			_, err := step(ctx, "aclose")
			return err
		},
	}
}

// Codec instance created in the child, its decode and encode run there
func (c *Client) codec(path string) (*tools.NemoRelayCodec, error) {
	v, err := c.call(context.Background(), path, nil, nil, false)
	if err != nil {
		return nil, err
	}
	o, ok := v.(*Object)
	if !ok {
		return nil, &tools.RelayError{Class: "TypeError", Message: path + " did not return a codec object"}
	}
	codec := &tools.NemoRelayCodec{
		Decode: func(ctx context.Context, request any) (any, error) {
			return c.call(ctx, map[string]any{"ref": o.id, "method": "decode"}, []any{request}, nil, false)
		},
		Encode: func(ctx context.Context, decoded, request any) (any, error) {
			return c.call(ctx, map[string]any{"ref": o.id, "method": "encode"}, []any{decoded, request}, nil, false)
		},
	}
	key := weak.Make(codec)
	c.mu.Lock()
	c.codecs[key] = o
	c.mu.Unlock()
	runtime.AddCleanup(codec, func(k weak.Pointer[tools.NemoRelayCodec]) {
		c.mu.Lock()
		delete(c.codecs, k)
		c.mu.Unlock()
	}, key)
	return codec, nil
}

var (
	loadMu     sync.Mutex
	loaded     *Client
	loadFailed struct {
		python string
		stamp  time.Time
		err    error
	}
)

// Interpreter of the dependency environment the binding hosts, the selected venv's python
func environmentPython(ctx context.Context) (string, error) {
	venv, err := pm.SelectedVenv(ctx, pm.RepoRoot())
	if err != nil {
		return "", err
	}
	return pm.VenvPython(venv), nil
}

// Site directory stamp so a package installed after a failed import is seen on the next load
func siteStamp(python string) time.Time {
	site, _ := pm.SitePackages(filepath.Dir(filepath.Dir(python)))
	st, err := os.Stat(site)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// Imports nemo_relay in the relay host child of the selected environment, provisioning the runtime set on first use
func Load() (*tools.NemoRelay, error) {
	ctx := context.Background()
	python, err := environmentPython(ctx)
	if err != nil {
		return nil, err
	}
	relay, err := LoadFrom(python)
	if !errors.Is(err, errNoRelayModule) || !wheelPinned() {
		return relay, err
	}
	if _, perr := pm.EnsureRuntimeEnvironment(ctx, pm.RuntimeEnvironmentOptions{}); perr != nil {
		slog.Warn("NeMo Relay runtime provisioning failed", "err", perr)
		return nil, err
	}
	if python, err = environmentPython(ctx); err != nil {
		return nil, err
	}
	loadMu.Lock()
	loadFailed.err = nil
	loadMu.Unlock()
	return LoadFrom(python)
}

// Imports nemo_relay in a relay host child of python, reusing its running child and starting one after an exit
func LoadFrom(python string) (*tools.NemoRelay, error) {
	loadMu.Lock()
	defer loadMu.Unlock()
	if loaded != nil && loaded.python == python && loaded.Alive() {
		return loaded.relay, nil
	}
	client, err := startEnvironment(context.Background(), python)
	if err != nil {
		return nil, err
	}
	loaded = client
	return client.relay, nil
}

// Missing module error of an environment without nemo_relay, the message the Python import raises
var errNoRelayModule = errors.New("No module named 'nemo_relay'")

// Whether the lock pins a nemo-relay wheel for this host, the hermes marker deciding where Relay exists
func wheelPinned() bool {
	target, err := pm.CurrentTarget()
	if err != nil {
		return false
	}
	_, ok := pm.NemoRelayWheel(target)
	return ok
}

// Starts the child over the selected environment, remembering a failed import until site-packages changes
func startEnvironment(ctx context.Context, python string) (*Client, error) {
	if st, err := os.Stat(python); err != nil || st.IsDir() {
		return nil, errNoRelayModule
	}
	stamp := siteStamp(python)
	if loadFailed.err != nil && loadFailed.python == python && loadFailed.stamp.Equal(stamp) {
		return nil, loadFailed.err
	}
	client, err := Start(ctx, python)
	if err != nil {
		var re *tools.RelayError
		if errors.As(err, &re) && re.Class == "ModuleNotFoundError" && re.Message == errNoRelayModule.Error() {
			err = errNoRelayModule
		}
		loadFailed.python, loadFailed.stamp, loadFailed.err = python, stamp, err
		return nil, err
	}
	loadFailed.err = nil
	return client, nil
}
