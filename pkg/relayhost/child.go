package relayhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Hidden verb of the binary that hosts nemo_relay in an embedded interpreter
const Entry = "relay-host"

const (
	exitStartFailed    = 3
	exitFinalizeFailed = 4
	exitFinalizeHung   = 5
	finalizeDeadline   = 20 * time.Second
)

var entryDispatched atomic.Bool

// Runs the relay host child when this binary was started as one, exiting with its status
func DispatchRelayHostEntry(ctx context.Context) {
	entryDispatched.Store(true)
	if len(os.Args) > 1 && os.Args[1] == Entry {
		os.Exit(runChild(ctx, os.Args[2:]))
	}
}

// Starts the named interpreter and serves the parent until it closes the channel
func runChild(ctx context.Context, args []string) int {
	if len(args) != 2 || args[0] != "--python" {
		fmt.Fprintf(os.Stderr, "relay host: usage: %s --python <interpreter>\n", Entry)
		return exitStartFailed
	}
	reader, writer, err := takeProtocolStreams()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay host: %v\n", err)
		return exitStartFailed
	}
	in, err := pyhost.Discover(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay host: %v\n", err)
		return exitStartFailed
	}
	h, err := pyhost.Start(pyhost.Options{Interpreter: in, Argv: []string{args[1]}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay host: %v\n", err)
		return exitStartFailed
	}
	c := &child{h: h, refs: map[int64]*pyhost.Ref{}, runs: map[int64]*childRun{}, excClasses: map[string]*pyhost.Ref{}}
	if err := h.Do(c.setup); err != nil {
		fmt.Fprintf(os.Stderr, "relay host: %v\n", err)
		return finalize(h, exitStartFailed)
	}
	c.c = newConn(reader, writer, true, c.serve)
	go c.c.readLoop()
	select {
	case <-c.c.done:
	case <-ctx.Done():
	}
	return finalize(h, 0)
}

// Finalizes the interpreter, a native thread holding it past the deadline ending the process
func finalize(h *pyhost.Host, code int) int {
	timer := time.AfterFunc(finalizeDeadline, func() {
		fmt.Fprintf(os.Stderr, "relay host: Py_FinalizeEx did not return within %s\n", finalizeDeadline)
		os.Exit(exitFinalizeHung)
	})
	rc, err := h.Finalize(finalizeDeadline)
	timer.Stop()
	if (err != nil || rc != 0) && code == 0 {
		fmt.Fprintf(os.Stderr, "relay host: Py_FinalizeEx returned %d: %v\n", rc, err)
		return exitFinalizeFailed
	}
	return code
}

// Interpreter side state of the relay host, Python objects the parent holds by id
type child struct {
	h          *pyhost.Host
	c          *conn
	mu         sync.Mutex
	refs       map[int64]*pyhost.Ref
	nextRef    int64
	runs       map[int64]*childRun
	excClasses map[string]*pyhost.Ref
	relay      *pyhost.Ref
	asyncio    *pyhost.Ref
	contextvar *pyhost.Ref
	serving    *pyhost.Ref
	environ    *pyhost.Ref
	osModule   *pyhost.Ref
	exception  *pyhost.Ref
	callable   *pyhost.Ref
	iterator   *pyhost.Ref
}

// Loop and future of one awaiting run op, the targets of a parent cancel
type childRun struct {
	loop, future, task *pyhost.Ref
}

// Scope owning the new references an op body creates
type pyScope struct {
	g    *pyhost.GIL
	held []uintptr
}

func (s *pyScope) keep(o uintptr, err error) (uintptr, error) {
	if err == nil && o != 0 {
		s.held = append(s.held, o)
	}
	return o, err
}

func (s *pyScope) close() {
	for _, o := range s.held {
		s.g.Decref(o)
	}
}

func (s *pyScope) attr(o uintptr, name string) (uintptr, error) { return s.keep(s.g.Attr(o, name)) }

func (s *pyScope) call(fn uintptr, args []any, kwargs map[string]any) (uintptr, error) {
	return s.keep(s.g.Call(fn, args, kwargs))
}

func (s *pyScope) method(o uintptr, name string, args []any, kwargs map[string]any) (uintptr, error) {
	return s.keep(s.g.CallMethod(o, name, args, kwargs))
}

// Imports the modules and builds the helper classes every op uses
func (c *child) setup(g *pyhost.GIL) error {
	own := func(o uintptr, err error) (*pyhost.Ref, error) {
		if err != nil {
			return nil, err
		}
		return g.Own(o), nil
	}
	var err error
	if c.asyncio, err = own(g.Import("asyncio")); err != nil {
		return err
	}
	contextvars, err := own(g.Import("contextvars"))
	if err != nil {
		return err
	}
	if c.contextvar, err = own(g.Attr(contextvars.Ptr(), "Context")); err != nil {
		return err
	}
	if c.serving, err = own(g.CallMethod(contextvars.Ptr(), "ContextVar", []any{"icebath_relay_serving"}, map[string]any{"default": int64(0)})); err != nil {
		return err
	}
	if c.osModule, err = own(g.Import("os")); err != nil {
		return err
	}
	if c.environ, err = own(g.Attr(c.osModule.Ptr(), "environ")); err != nil {
		return err
	}
	builtins, err := own(g.Import("builtins"))
	if err != nil {
		return err
	}
	if c.exception, err = own(g.Attr(builtins.Ptr(), "Exception")); err != nil {
		return err
	}
	if c.callable, err = own(g.NewClass("icebath_relay", "ParentCallable", nil, map[string]any{
		"__call__": pyhost.RawFunc(c.callableCall),
		"__del__":  pyhost.RawFunc(c.releaseParent),
	})); err != nil {
		return err
	}
	c.iterator, err = own(g.NewClass("icebath_relay", "ParentAsyncIterator", nil, map[string]any{
		"__aiter__": pyhost.RawFunc(func(g *pyhost.GIL, args, _ uintptr) (uintptr, error) {
			self := g.TupleItem(args, 0)
			return g.ToPython(pyhost.Borrowed(self))
		}),
		"__anext__": pyhost.RawFunc(func(g *pyhost.GIL, args, _ uintptr) (uintptr, error) {
			id, err := parentID(g, g.TupleItem(args, 0))
			if err != nil {
				return 0, err
			}
			return c.iteratorStep(g, id, "iter_next")
		}),
		"__del__": pyhost.RawFunc(c.releaseParent),
	}))
	return err
}

// Parent id an instance of a helper class stands for
func parentID(g *pyhost.GIL, self uintptr) (int64, error) {
	o, err := g.Attr(self, "_icebath_id")
	if err != nil {
		return 0, err
	}
	defer g.Decref(o)
	v, err := g.FromPython(o)
	if err != nil {
		return 0, err
	}
	id, _ := v.(int64)
	return id, nil
}

// Calls the parent callback an instance stands for, events projected to their public attributes
func (c *child) callableCall(g *pyhost.GIL, args, kwargs uintptr) (uintptr, error) {
	self := g.TupleItem(args, 0)
	id, err := parentID(g, self)
	if err != nil {
		return 0, err
	}
	kindObj, err := g.Attr(self, "_icebath_kind")
	if err != nil {
		return 0, err
	}
	kind, _ := g.Str(kindObj)
	g.Decref(kindObj)
	s := &pyScope{g: g}
	defer s.close()
	all, err := g.FromPython(args)
	if err != nil {
		return 0, err
	}
	rest := all.([]any)[1:]
	if self, ok := all.([]any)[0].(*pyhost.Ref); ok {
		self.Release()
	}
	var lowered []any
	if kind == "event" && len(rest) == 1 {
		ref, _ := rest[0].(*pyhost.Ref)
		if ref == nil {
			return 0, &pyhost.Exception{Type: "TypeError", Message: "subscriber event is not an object"}
		}
		event, err := c.eventView(g, ref.Ptr())
		if err != nil {
			return 0, err
		}
		lowered = []any{event}
	} else {
		for _, item := range rest {
			v, err := c.lowerPy(g, item)
			if err != nil {
				return 0, err
			}
			lowered = append(lowered, v)
		}
	}
	if kwargs != 0 {
		if n, _ := g.FromPython(kwargs); len(n.(map[string]any)) > 0 {
			return 0, &pyhost.Exception{Type: "TypeError", Message: "parent callbacks take positional arguments only"}
		}
	}
	result, err := c.callParent(g, "callback", map[string]any{"id": id, "args": lowered})
	if err != nil {
		return 0, err
	}
	return c.toPython(g, s, result)
}

// Coroutine running one blocking parent iterator step off the loop on a worker thread
func (c *child) iteratorStep(g *pyhost.GIL, id int64, op string) (uintptr, error) {
	step, err := g.NewFunc(op, func(g *pyhost.GIL, _ []any, _ map[string]any) (any, error) {
		s := &pyScope{g: g}
		defer s.close()
		result, err := c.callParent(g, op, map[string]any{"id": id})
		if err != nil {
			return nil, err
		}
		o, err := c.toPython(g, s, result)
		if err != nil {
			return nil, err
		}
		return g.Own(o), nil
	})
	if err != nil {
		return 0, err
	}
	defer g.Decref(step)
	return g.CallMethod(c.asyncio.Ptr(), "to_thread", []any{pyhost.Borrowed(step)}, nil)
}

// Tells the parent the Python side dropped the callback or iterator an instance stood for
func (c *child) releaseParent(g *pyhost.GIL, args, _ uintptr) (uintptr, error) {
	if id, err := parentID(g, g.TupleItem(args, 0)); err == nil && id != 0 && c.c != nil {
		raw, _ := json.Marshal(map[string]any{"ids": []int64{id}})
		_ = c.c.send(&message{Op: "release", Args: raw})
	}
	return g.ToPython(nil)
}

// Serving request id of the running context, the origin of a parent call made from here
func (c *child) servingID(g *pyhost.GIL) int64 {
	o, err := g.CallMethod(c.serving.Ptr(), "get", nil, nil)
	if err != nil {
		return 0
	}
	defer g.Decref(o)
	v, _ := g.FromPython(o)
	id, _ := v.(int64)
	return id
}

// Calls the parent and waits with the GIL released, running nested parent requests on this thread
func (c *child) callParent(g *pyhost.GIL, op string, args map[string]any) (any, error) {
	_, ch, err := c.c.open(op, c.servingID(g), args)
	if err != nil {
		return nil, err
	}
	for {
		var m *message
		var ok bool
		g.Unblocked(func() { m, ok = <-ch })
		if !ok {
			return nil, &pyhost.Exception{Type: "RuntimeError", Message: "NeMo Relay host parent closed the channel"}
		}
		if m.Op != "" {
			c.handle(g, m)
			continue
		}
		if m.Error != nil {
			return nil, c.raiseParentError(g, m.Error)
		}
		raw, err := decodeJSON(m.Result)
		if err != nil {
			return nil, err
		}
		return liftValue(raw, c.liftTag)
	}
}

// Python exception for a parent failure, a class named after the Go error type carrying its id
func (c *child) raiseParentError(g *pyhost.GIL, e *wireError) error {
	if e.Module == "" {
		return &pyhost.Exception{Type: e.Class, Message: e.Message}
	}
	key := e.Module + "." + e.Class
	c.mu.Lock()
	cls := c.excClasses[key]
	c.mu.Unlock()
	if cls == nil {
		o, err := g.NewClass(e.Module, e.Class, []any{pyhost.Borrowed(c.exception.Ptr())}, nil)
		if err != nil {
			return err
		}
		cls = g.Own(o)
		c.mu.Lock()
		c.excClasses[key] = cls
		c.mu.Unlock()
	}
	inst, err := g.Call(cls.Ptr(), []any{e.Message}, nil)
	if err != nil {
		return err
	}
	ref := g.Own(inst)
	if e.GoErr != 0 {
		if err := g.SetAttr(inst, "_icebath_goerr", e.GoErr); err != nil {
			return err
		}
	}
	return g.Raising(ref)
}

// Runs top level parent requests on their own goroutine holding the GIL
func (c *child) serve(m *message) {
	go func() {
		if err := c.h.Do(func(g *pyhost.GIL) error { c.handle(g, m); return nil }); err != nil {
			_ = c.c.reply(m.ID, nil, &wireError{Class: "RuntimeError", Message: err.Error()})
		}
	}()
}

// Runs one parent request on the calling thread and replies
func (c *child) handle(g *pyhost.GIL, m *message) {
	result, err := c.dispatch(g, m)
	if m.Op == "release" || m.Op == "cancel" {
		return
	}
	if err != nil {
		_ = c.c.reply(m.ID, nil, c.wireFailure(g, err))
		return
	}
	_ = c.c.reply(m.ID, result, nil)
}

// Wire form of an op failure, the parent error id kept when the exception came from a parent callback
func (c *child) wireFailure(g *pyhost.GIL, err error) *wireError {
	var we *wireError
	if errors.As(err, &we) {
		return we
	}
	var pe *pyhost.PyError
	if !errors.As(err, &pe) {
		return &wireError{Class: "RuntimeError", Message: err.Error()}
	}
	out := &wireError{Class: pe.Name(), Message: pe.Message}
	if exc := pe.Exception(); exc != nil && g.HasAttr(exc.Ptr(), "_icebath_goerr") {
		if o, err := g.Attr(exc.Ptr(), "_icebath_goerr"); err == nil {
			v, _ := g.FromPython(o)
			out.GoErr, _ = v.(int64)
			g.Decref(o)
		}
	}
	return out
}

func (c *child) dispatch(g *pyhost.GIL, m *message) (any, error) {
	raw, err := decodeJSON(m.Args)
	if err != nil {
		return nil, err
	}
	args, _ := raw.(map[string]any)
	switch m.Op {
	case "hello":
		return c.hello(g)
	case "call":
		return c.call(g, m.ID, args)
	case "run":
		return c.run(g, m.ID, args)
	case "attr":
		s := &pyScope{g: g}
		defer s.close()
		o, err := c.ref(wireInt(args["ref"]))
		if err != nil {
			return nil, err
		}
		name, _ := args["name"].(string)
		v, err := s.attr(o, name)
		if err != nil {
			return nil, err
		}
		return c.lowerObject(g, v)
	case "cancel":
		c.cancel(g, wireInt(args["id"]))
		return nil, nil
	case "release":
		ids, _ := args["ids"].([]any)
		var dropped []*pyhost.Ref
		c.mu.Lock()
		for _, id := range ids {
			if r := c.refs[wireInt(id)]; r != nil {
				dropped = append(dropped, r)
				delete(c.refs, wireInt(id))
			}
		}
		c.mu.Unlock()
		for _, r := range dropped {
			c.closeIdleLoop(g, r.Ptr())
			r.Release()
		}
		return nil, nil
	}
	return nil, &wireError{Class: "RuntimeError", Message: "unknown relay host op " + m.Op}
}

// Attribute paths of nemo_relay the binding surface reads, each reported when present
var helloPaths = []string{"scope", "scope.push", "scope.pop", "scope.get_handle", "scope.event", "ScopeType", "ScopeType.Agent",
	"ScopeType.Function", "get_scope_stack", "plugin", "plugin.initialize", "plugin.validate", "plugin.validate_exact",
	"subscribers", "subscribers.register", "subscribers.deregister", "subscribers.flush", "subscribers.flush_async",
	"tools", "tools.request_intercepts", "tools.execute", "tools.call", "tools.call_end", "llm", "llm.execute",
	"llm.stream_execute", "llm.call", "llm.call_end", "LLMRequest", "codecs", "codecs.OpenAIChatCodec",
	"codecs.OpenAIResponsesCodec", "codecs.AnthropicMessagesCodec", "ToolExecutionResult"}

// Imports nemo_relay and reports which binding attributes exist, the scope types and the pop signature
func (c *child) hello(g *pyhost.GIL) (any, error) {
	s := &pyScope{g: g}
	defer s.close()
	if c.relay == nil {
		mod, err := g.Import("nemo_relay")
		if err != nil {
			return nil, err
		}
		c.relay = g.Own(mod)
	}
	present := []any{}
	for _, path := range helloPaths {
		if o, err := c.resolve(g, s, path); err == nil && o != 0 {
			present = append(present, path)
		}
	}
	out := map[string]any{"paths": present}
	for _, member := range []string{"Agent", "Function"} {
		if o, err := c.resolve(g, s, "ScopeType."+member); err == nil {
			v, err := c.lowerObject(g, o)
			if err != nil {
				return nil, err
			}
			out[strings.ToLower(member)] = v
		}
	}
	if pop, err := c.resolve(g, s, "scope.pop"); err == nil {
		params, err := c.popParameters(g, s, pop)
		if err != nil {
			out["pop_error"] = c.wireFailure(g, err)
		} else {
			out["pop_params"] = params
		}
	}
	return out, nil
}

// Parameter names of scope.pop and whether each is the variadic keyword one
func (c *child) popParameters(g *pyhost.GIL, s *pyScope, pop uintptr) ([]any, error) {
	inspect, err := s.keep(g.Import("inspect"))
	if err != nil {
		return nil, err
	}
	sig, err := s.method(inspect, "signature", []any{pyhost.Borrowed(pop)}, nil)
	if err != nil {
		return nil, err
	}
	params, err := s.attr(sig, "parameters")
	if err != nil {
		return nil, err
	}
	values, err := s.method(params, "values", nil, nil)
	if err != nil {
		return nil, err
	}
	paramCls, err := s.attr(inspect, "Parameter")
	if err != nil {
		return nil, err
	}
	varKeyword, err := s.attr(paramCls, "VAR_KEYWORD")
	if err != nil {
		return nil, err
	}
	out := []any{}
	err = g.Iterate(values, func(item uintptr) error {
		name, err := s.attr(item, "name")
		if err != nil {
			return err
		}
		kind, err := s.attr(item, "kind")
		if err != nil {
			return err
		}
		text, err := g.Str(name)
		if err != nil {
			return err
		}
		out = append(out, []any{text, kind == varKeyword})
		return nil
	})
	return out, err
}

// Attribute path under nemo_relay, a borrowed object kept alive by the scope
func (c *child) resolve(g *pyhost.GIL, s *pyScope, path string) (uintptr, error) {
	if c.relay == nil {
		return 0, &pyhost.Exception{Type: "ModuleNotFoundError", Message: "No module named 'nemo_relay'"}
	}
	o := c.relay.Ptr()
	for _, part := range strings.Split(path, ".") {
		next, err := s.attr(o, part)
		if err != nil {
			return 0, err
		}
		o = next
	}
	return o, nil
}

func (c *child) ref(id int64) (uintptr, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.refs[id]
	if r == nil {
		return 0, &pyhost.Exception{Type: "ReferenceError", Message: fmt.Sprintf("NeMo Relay host object %d was released", id)}
	}
	return r.Ptr(), nil
}

func (c *child) keepRef(g *pyhost.GIL, o uintptr) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextRef++
	c.refs[c.nextRef] = g.NewRef(o)
	return c.nextRef
}

// Callable an op targets, a nemo_relay attribute path or a method of a held object, or the object itself
func (c *child) target(g *pyhost.GIL, s *pyScope, fn any) (uintptr, error) {
	switch x := fn.(type) {
	case string:
		return c.resolve(g, s, x)
	case map[string]any:
		o, err := c.ref(wireInt(x["ref"]))
		if err != nil {
			return 0, err
		}
		if method, _ := x["method"].(string); method != "" {
			return s.attr(o, method)
		}
		return o, nil
	}
	return 0, &pyhost.Exception{Type: "TypeError", Message: "relay host op has no target"}
}

// Fresh copy of the context the parent named, an empty context when it named none
func (c *child) context(g *pyhost.GIL, s *pyScope, id int64, opID int64) (uintptr, error) {
	var ctx uintptr
	var err error
	if id == 0 {
		ctx, err = s.call(c.contextvar.Ptr(), nil, nil)
	} else {
		base, rerr := c.ref(id)
		if rerr != nil {
			return 0, rerr
		}
		ctx, err = s.method(base, "copy", nil, nil)
	}
	if err != nil {
		return 0, err
	}
	setter, err := s.attr(c.serving.Ptr(), "set")
	if err != nil {
		return 0, err
	}
	if _, err := s.method(ctx, "run", []any{pyhost.Borrowed(setter), opID}, nil); err != nil {
		return 0, err
	}
	return ctx, nil
}

// Positional and keyword arguments of an op converted to Python
func (c *child) callArgs(g *pyhost.GIL, s *pyScope, args map[string]any) ([]any, map[string]any, error) {
	positional, _ := args["args"].([]any)
	keywords, _ := args["kwargs"].(map[string]any)
	pos, err := liftValue(positional, c.liftTag)
	if err != nil {
		return nil, nil, err
	}
	kw, err := liftValue(keywords, c.liftTag)
	if err != nil {
		return nil, nil, err
	}
	outPos := []any{}
	if list, ok := pos.([]any); ok {
		for _, item := range list {
			v, err := c.pythonArg(g, s, item)
			if err != nil {
				return nil, nil, err
			}
			outPos = append(outPos, v)
		}
	}
	outKw := map[string]any{}
	if m, ok := kw.(map[string]any); ok {
		for k, item := range m {
			v, err := c.pythonArg(g, s, item)
			if err != nil {
				return nil, nil, err
			}
			outKw[k] = v
		}
	}
	return outPos, outKw, nil
}

// Applies the parent's environment and working directory before plugin discovery reads them
func (c *child) applyEnvironment(g *pyhost.GIL, args map[string]any) error {
	env, ok := args["env"].(map[string]any)
	if !ok {
		return nil
	}
	if _, err := g.CallMethod(c.environ.Ptr(), "clear", nil, nil); err != nil {
		return err
	}
	o, err := g.CallMethod(c.environ.Ptr(), "update", []any{env}, nil)
	if err != nil {
		return err
	}
	g.Decref(o)
	if cwd, _ := args["cwd"].(string); cwd != "" {
		o, err := g.CallMethod(c.osModule.Ptr(), "chdir", []any{cwd}, nil)
		if err != nil {
			return err
		}
		g.Decref(o)
	}
	return nil
}

// Synchronous call inside a copy of the parent's context, returning the value and the context it left
func (c *child) call(g *pyhost.GIL, opID int64, args map[string]any) (any, error) {
	s := &pyScope{g: g}
	defer s.close()
	if err := c.applyEnvironment(g, args); err != nil {
		return nil, err
	}
	ctx, err := c.context(g, s, wireInt(args["ctx"]), opID)
	if err != nil {
		return nil, err
	}
	fn, err := c.target(g, s, args["fn"])
	if err != nil {
		return nil, err
	}
	pos, kw, err := c.callArgs(g, s, args)
	if err != nil {
		return nil, err
	}
	result, err := s.method(ctx, "run", append([]any{pyhost.Borrowed(fn)}, pos...), kw)
	if err != nil {
		return nil, err
	}
	value, err := c.lowerObject(g, result)
	if err != nil {
		return nil, err
	}
	return map[string]any{"value": value, "ctx": c.keepRef(g, ctx)}, nil
}

// Awaits a call on an event loop inside a copy of the parent's context, the way asyncio.run drives a coroutine
func (c *child) run(g *pyhost.GIL, opID int64, args map[string]any) (any, error) {
	running, err := g.CallMethod(c.asyncio.Ptr(), "_get_running_loop", nil, nil)
	if err != nil {
		return nil, err
	}
	busy := !g.IsNone(running)
	g.Decref(running)
	if !busy {
		return c.runHere(g, opID, args)
	}
	var out any
	var outErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := c.h.Do(func(g *pyhost.GIL) error { out, outErr = c.runHere(g, opID, args); return nil }); err != nil {
			outErr = err
		}
	}()
	g.Unblocked(func() { <-done })
	return out, outErr
}

func (c *child) runHere(g *pyhost.GIL, opID int64, args map[string]any) (any, error) {
	s := &pyScope{g: g}
	defer s.close()
	if err := c.applyEnvironment(g, args); err != nil {
		return nil, err
	}
	ctx, err := c.context(g, s, wireInt(args["ctx"]), opID)
	if err != nil {
		return nil, err
	}
	fn, err := c.target(g, s, args["fn"])
	if err != nil {
		return nil, err
	}
	pos, kw, err := c.callArgs(g, s, args)
	if err != nil {
		return nil, err
	}
	invoke, _ := args["invoke"].(bool)
	loopMode := wireInt(args["loop"])
	var loop uintptr
	switch loopMode {
	case 0, -1:
		loop, err = s.method(c.asyncio.Ptr(), "new_event_loop", nil, nil)
	default:
		loop, err = c.ref(loopMode)
	}
	if err != nil {
		return nil, err
	}
	failed := true
	if loopMode == 0 {
		defer c.closeLoop(g, loop)
	} else if loopMode == -1 {
		defer func() {
			if failed {
				c.closeLoop(g, loop)
			}
		}()
	}
	future, err := s.method(loop, "create_future", nil, nil)
	if err != nil {
		return nil, err
	}
	state := &childRun{loop: g.NewRef(loop), future: g.NewRef(future)}
	c.mu.Lock()
	c.runs[opID] = state
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.runs, opID)
		c.mu.Unlock()
		state.loop.Release()
		state.future.Release()
		if state.task != nil {
			state.task.Release()
		}
	}()
	starter, err := s.keep(g.NewFunc("relay_start", func(g *pyhost.GIL, _ []any, _ map[string]any) (any, error) {
		return nil, c.start(g, loop, future, fn, invoke, pos, kw, state)
	}))
	if err != nil {
		return nil, err
	}
	if _, err := s.method(loop, "call_soon", []any{pyhost.Borrowed(starter)}, map[string]any{"context": pyhost.Borrowed(ctx)}); err != nil {
		return nil, err
	}
	result, err := s.method(loop, "run_until_complete", []any{pyhost.Borrowed(future)}, nil)
	if err != nil {
		return nil, err
	}
	value, err := c.lowerObject(g, result)
	if err != nil {
		return nil, err
	}
	failed = false
	out := map[string]any{"value": value}
	if loopMode == -1 {
		out["loop"] = c.keepRef(g, loop)
	}
	return out, nil
}

// Calls the target on the running loop and chains the awaitable it returns into the op future
func (c *child) start(g *pyhost.GIL, loop, future, fn uintptr, invoke bool, pos []any, kw map[string]any, state *childRun) error {
	s := &pyScope{g: g}
	defer s.close()
	settle := func(name string, value any) error {
		_, err := s.method(future, name, []any{value}, nil)
		return err
	}
	value := fn
	if invoke {
		v, err := s.call(fn, pos, kw)
		if err != nil {
			var pe *pyhost.PyError
			if errors.As(err, &pe) && pe.Exception() != nil {
				return settle("set_exception", pyhost.Borrowed(pe.Exception().Ptr()))
			}
			return err
		}
		value = v
	}
	awaitable, err := g.IsAwaitable(value)
	if err != nil {
		return err
	}
	if !awaitable {
		return settle("set_result", pyhost.Borrowed(value))
	}
	task, err := s.method(c.asyncio.Ptr(), "ensure_future", []any{pyhost.Borrowed(value)}, map[string]any{"loop": pyhost.Borrowed(loop)})
	if err != nil {
		return err
	}
	c.mu.Lock()
	state.task = g.NewRef(task)
	c.mu.Unlock()
	chain, err := s.keep(g.NewRawFunc("relay_chain", func(g *pyhost.GIL, args, _ uintptr) (uintptr, error) {
		return c.chain(g, future, g.TupleItem(args, 0))
	}))
	if err != nil {
		return err
	}
	_, err = s.method(task, "add_done_callback", []any{pyhost.Borrowed(chain)}, nil)
	return err
}

// Copies a finished task's outcome into the op future
func (c *child) chain(g *pyhost.GIL, future, task uintptr) (uintptr, error) {
	s := &pyScope{g: g}
	defer s.close()
	done, err := s.method(future, "done", nil, nil)
	if err != nil {
		return 0, err
	}
	if finished, _ := g.Truthy(done); finished {
		return g.ToPython(nil)
	}
	cancelled, err := s.method(task, "cancelled", nil, nil)
	if err != nil {
		return 0, err
	}
	if yes, _ := g.Truthy(cancelled); yes {
		if _, err := s.method(future, "cancel", nil, nil); err != nil {
			return 0, err
		}
		return g.ToPython(nil)
	}
	exc, err := s.method(task, "exception", nil, nil)
	if err != nil {
		return 0, err
	}
	if !g.IsNone(exc) {
		if _, err := s.method(future, "set_exception", []any{pyhost.Borrowed(exc)}, nil); err != nil {
			return 0, err
		}
		return g.ToPython(nil)
	}
	result, err := s.method(task, "result", nil, nil)
	if err != nil {
		return 0, err
	}
	if _, err := s.method(future, "set_result", []any{pyhost.Borrowed(result)}, nil); err != nil {
		return 0, err
	}
	return g.ToPython(nil)
}

// Cancels what is left on a fresh loop, shuts down its generators and executor and closes it
func (c *child) closeLoop(g *pyhost.GIL, loop uintptr) {
	s := &pyScope{g: g}
	defer s.close()
	tasks, err := s.method(c.asyncio.Ptr(), "all_tasks", []any{pyhost.Borrowed(loop)}, nil)
	if err == nil {
		var pending []any
		_ = g.Iterate(tasks, func(item uintptr) error {
			if _, err := s.method(item, "cancel", nil, nil); err != nil {
				return err
			}
			pending = append(pending, pyhost.Borrowed(item))
			return nil
		})
		if len(pending) > 0 {
			if gathered, err := s.method(c.asyncio.Ptr(), "gather", pending, map[string]any{"return_exceptions": true}); err == nil {
				_, _ = s.method(loop, "run_until_complete", []any{pyhost.Borrowed(gathered)}, nil)
			}
		}
	}
	for _, name := range []string{"shutdown_asyncgens", "shutdown_default_executor"} {
		if coro, err := s.method(loop, name, nil, nil); err == nil {
			_, _ = s.method(loop, "run_until_complete", []any{pyhost.Borrowed(coro)}, nil)
		}
	}
	_, _ = s.method(loop, "close", nil, nil)
}

// Closes a released event loop the parent kept for a stream once nothing runs it
func (c *child) closeIdleLoop(g *pyhost.GIL, o uintptr) {
	s := &pyScope{g: g}
	defer s.close()
	base, err := s.attr(c.asyncio.Ptr(), "AbstractEventLoop")
	if err != nil {
		return
	}
	if yes, _ := g.IsInstance(o, base); !yes {
		return
	}
	for _, probe := range []string{"is_running", "is_closed"} {
		v, err := s.method(o, probe, nil, nil)
		if err != nil {
			return
		}
		if yes, _ := g.Truthy(v); yes {
			return
		}
	}
	c.closeLoop(g, o)
}

// Cancels the task and future of an awaiting op from any thread
func (c *child) cancel(g *pyhost.GIL, id int64) {
	c.mu.Lock()
	state := c.runs[id]
	c.mu.Unlock()
	if state == nil {
		return
	}
	s := &pyScope{g: g}
	defer s.close()
	for _, target := range []*pyhost.Ref{state.task, state.future} {
		if target == nil {
			continue
		}
		if cancel, err := s.attr(target.Ptr(), "cancel"); err == nil {
			_, _ = s.method(state.loop.Ptr(), "call_soon_threadsafe", []any{pyhost.Borrowed(cancel)}, nil)
		}
	}
}

// Python argument for a lifted wire value, creating the objects tagged nodes stand for
func (c *child) pythonArg(g *pyhost.GIL, s *pyScope, v any) (any, error) {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			conv, err := c.pythonArg(g, s, item)
			if err != nil {
				return nil, err
			}
			out[i] = conv
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			conv, err := c.pythonArg(g, s, item)
			if err != nil {
				return nil, err
			}
			out[k] = conv
		}
		return out, nil
	case pendingObject:
		o, err := c.materialize(g, s, x)
		if err != nil {
			return nil, err
		}
		return pyhost.Borrowed(o), nil
	}
	return v, nil
}

// Tagged node waiting for the GIL to become a Python object
type pendingObject struct {
	tag  string
	body any
}

func (c *child) liftTag(tag string, body any) (any, error) {
	switch tag {
	case "$r", "$c", "$x", "$q", "$t", "$o", "$m":
		if tag == "$q" || tag == "$t" || tag == "$o" {
			lifted, err := liftValue(body, c.liftTag)
			if err != nil {
				return nil, err
			}
			body = lifted
		}
		return pendingObject{tag: tag, body: body}, nil
	}
	return nil, fmt.Errorf("ValueError: unknown wire tag %s", tag)
}

// Python object of a tagged node, a held object, a parent callable or iterator, or a nemo_relay value class
func (c *child) materialize(g *pyhost.GIL, s *pyScope, p pendingObject) (uintptr, error) {
	switch p.tag {
	case "$r":
		return c.ref(wireInt(p.body))
	case "$c", "$x":
		parts, _ := p.body.([]any)
		if len(parts) == 0 {
			return 0, &pyhost.Exception{Type: "ValueError", Message: "parent object without id"}
		}
		cls := c.callable
		if p.tag == "$x" {
			cls = c.iterator
		}
		inst, err := s.call(cls.Ptr(), nil, nil)
		if err != nil {
			return 0, err
		}
		if err := g.SetAttr(inst, "_icebath_id", wireInt(parts[0])); err != nil {
			return 0, err
		}
		if p.tag == "$c" && len(parts) > 1 {
			if err := g.SetAttr(inst, "_icebath_kind", parts[1]); err != nil {
				return 0, err
			}
		}
		if p.tag == "$x" && len(parts) > 1 && parts[1] == true {
			id := wireInt(parts[0])
			closer, err := s.keep(g.NewRawFunc("aclose", func(g *pyhost.GIL, _, _ uintptr) (uintptr, error) {
				return c.iteratorStep(g, id, "iter_close")
			}))
			if err != nil {
				return 0, err
			}
			if err := g.SetAttr(inst, "aclose", pyhost.Borrowed(closer)); err != nil {
				return 0, err
			}
		}
		return inst, nil
	case "$q":
		parts, _ := p.body.([]any)
		if len(parts) != 2 {
			return 0, &pyhost.Exception{Type: "ValueError", Message: "LLMRequest needs headers and content"}
		}
		cls, err := c.resolve(g, s, "LLMRequest")
		if err != nil {
			return 0, err
		}
		headers, err := c.pythonArg(g, s, parts[0])
		if err != nil {
			return 0, err
		}
		content, err := c.pythonArg(g, s, parts[1])
		if err != nil {
			return 0, err
		}
		return s.call(cls, []any{headers, content}, nil)
	case "$t":
		cls, err := c.resolve(g, s, "ToolExecutionResult")
		if err != nil {
			return 0, err
		}
		result, err := c.pythonArg(g, s, p.body)
		if err != nil {
			return 0, err
		}
		return s.call(cls, []any{result}, nil)
	case "$o":
		pairs, err := c.pythonArg(g, s, p.body)
		if err != nil {
			return 0, err
		}
		builtins, err := s.keep(g.Import("builtins"))
		if err != nil {
			return 0, err
		}
		return s.method(builtins, "dict", []any{pairs}, nil)
	case "$m":
		datetime, err := s.keep(g.Import("datetime"))
		if err != nil {
			return 0, err
		}
		cls, err := s.attr(datetime, "datetime")
		if err != nil {
			return 0, err
		}
		return s.method(cls, "fromisoformat", []any{p.body}, nil)
	}
	return 0, &pyhost.Exception{Type: "ValueError", Message: "unknown wire object " + p.tag}
}

// Python object for a value the parent returned, a new reference
func (c *child) toPython(g *pyhost.GIL, s *pyScope, v any) (uintptr, error) {
	arg, err := c.pythonArg(g, s, v)
	if err != nil {
		return 0, err
	}
	return g.ToPython(arg)
}

// Wire tree of a Python object
func (c *child) lowerObject(g *pyhost.GIL, o uintptr) (any, error) {
	v, err := g.FromPython(o)
	if err != nil {
		return nil, err
	}
	return c.lowerPy(g, v)
}

// Wire tree of a converted Python value, nemo_relay value classes by content and other objects by id
func (c *child) lowerPy(g *pyhost.GIL, v any) (any, error) {
	return lowerValue(v, 0, func(v any, depth int) (any, bool, error) {
		ref, ok := v.(*pyhost.Ref)
		if !ok {
			return nil, false, nil
		}
		s := &pyScope{g: g}
		defer s.close()
		for _, kind := range []struct{ path, tag string }{{"LLMRequest", "$q"}, {"ToolExecutionResult", "$t"}} {
			cls, err := c.resolve(g, s, kind.path)
			if err != nil {
				continue
			}
			if yes, _ := g.IsInstance(ref.Ptr(), cls); !yes {
				continue
			}
			if kind.tag == "$q" {
				headers, err := c.lowerAttr(g, s, ref.Ptr(), "headers")
				if err != nil {
					return nil, true, err
				}
				content, err := c.lowerAttr(g, s, ref.Ptr(), "content")
				if err != nil {
					return nil, true, err
				}
				return map[string]any{"$q": []any{headers, content}}, true, nil
			}
			result, err := c.lowerAttr(g, s, ref.Ptr(), "result")
			if err != nil {
				return nil, true, err
			}
			return map[string]any{"$t": result}, true, nil
		}
		awaitable, _ := g.IsAwaitable(ref.Ptr())
		var uuid any
		if g.HasAttr(ref.Ptr(), "uuid") {
			if u, err := s.attr(ref.Ptr(), "uuid"); err == nil {
				if text, err := g.FromPython(u); err == nil {
					if str, ok := text.(string); ok {
						uuid = str
					}
				}
			}
		}
		id := c.keepRef(g, ref.Ptr())
		ref.Release()
		return map[string]any{"$r": []any{id, uuid, awaitable}}, true, nil
	})
}

func (c *child) lowerAttr(g *pyhost.GIL, s *pyScope, o uintptr, name string) (any, error) {
	v, err := s.attr(o, name)
	if err != nil {
		return nil, err
	}
	return c.lowerObject(g, v)
}

// Public attributes of a Relay event the parent subscriber reads
var eventFields = []string{"kind", "name", "category", "scope_category", "category_profile", "metadata", "data"}

// Event projected to the attributes a subscriber reads, a missing attribute as None
func (c *child) eventView(g *pyhost.GIL, o uintptr) (any, error) {
	s := &pyScope{g: g}
	defer s.close()
	out := map[string]any{}
	for _, name := range eventFields {
		if !g.HasAttr(o, name) {
			out[name] = nil
			continue
		}
		v, err := s.attr(o, name)
		if err != nil {
			return nil, err
		}
		lowered, err := c.lowerObject(g, v)
		if err != nil {
			return nil, err
		}
		out[name] = lowered
	}
	return map[string]any{"$e": out}, nil
}
