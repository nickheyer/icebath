package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Reference tables, Python helper classes and the channel of one plugin host child
type runtime struct {
	h  *pyhost.Host
	ch *plugins.Channel

	mu              sync.Mutex
	nextID          int64
	refs            map[int64]Ref
	owners          map[int64]string
	iterators       map[int64]Ref
	predeclared     map[string]Ref
	unloadCallbacks map[string][]Ref
	unloadOrder     []string
	modules         map[string]string
	asgiApps        map[string]Ref
	profiles        map[string]*profileSet
	instanceModules map[string]Ref
	entryPoints     map[string]Ref
	released        []int64

	stopped  chan struct{}
	stopOnce sync.Once

	serving          Ref
	logger           Ref
	recordClass      Ref
	opaqueClass      Ref
	parentCallClass  Ref
	parentEventClass Ref
	registrationCls  Ref
	hostErrorClass   Ref
	unavailableClass Ref
	unsupportedClass Ref
	sdkModule        Ref
	asyncMethodClass Ref
	cliParserCls     Ref
	parentObjClasses map[string]Ref

	captureLock       Ref
	capturingRegister Ref
	originalRegister  Ref
	captures          [][]Ref
	holds             map[int64]*heldManagers
}

// Profiles a plugin directory or entry point registered, by name in registration order
type profileSet struct {
	order  []string
	byName map[string]Ref
}

func (s *profileSet) add(p py, profile Ref) {
	name := p.str(p.get(profile, "name"))
	if _, ok := s.byName[name]; !ok {
		s.order = append(s.order, name)
	}
	s.byName[name] = p.keep(profile)
}

func newRuntime(h *pyhost.Host) *runtime {
	return &runtime{h: h, refs: map[int64]Ref{}, owners: map[int64]string{}, iterators: map[int64]Ref{},
		predeclared: map[string]Ref{}, unloadCallbacks: map[string][]Ref{}, modules: map[string]string{},
		asgiApps: map[string]Ref{}, profiles: map[string]*profileSet{}, instanceModules: map[string]Ref{},
		entryPoints: map[string]Ref{}, holds: map[int64]*heldManagers{}, stopped: make(chan struct{})}
}

// Builds the helper classes, the serving context variable and the Go backed SDK channel module
func (rt *runtime) setup(p py) {
	rt.serving = p.mKw(p.imp("contextvars"), "ContextVar", []any{"plugin_host_serving"}, map[string]any{"default": nil})
	rt.logger = p.m(p.imp("logging"), "getLogger", "icebath_cli.plugin_host_child")
	rt.setupClasses(p)
	rt.installSDK(p)
}

// Class created through type() with Go method bodies
func (rt *runtime) class(p py, module, name string, bases []any, members map[string]any) Ref {
	if bases == nil {
		bases = []any{}
	}
	return p.own(p.g.NewClass(module, name, bases, members))
}

// Installs the Go backed icebath_cli.plugin_sdk whose parent_call sends sdk requests to the parent
func (rt *runtime) installSDK(p py) {
	module := p.call(p.attr("types", "ModuleType"), "icebath_cli.plugin_sdk")
	p.set(module, "parent_call", p.fn("parent_call", func(p py, args []any, kw map[string]any) any {
		if len(args) == 0 {
			throw(&pyhost.Exception{Type: "TypeError", Message: "parent_call() missing 1 required positional argument: 'op'"})
		}
		op := str(args[0])
		refs := func(p py, o Ref) map[string]any { return rt.eventRef(p, o, "") }
		payload := map[string]any{"op": op, "args": rt.encode(p, p.obj(args[1:]), refs), "kwargs": rt.encode(p, p.obj(kwOrEmpty(kw)), refs)}
		result := rt.send(p, "sdk", payload)
		return rt.decode(p, result, rt.resolveEventsOnly)
	}))
	p.m(p.get(p.imp("sys"), "modules"), "__setitem__", "icebath_cli.plugin_sdk", module)
	rt.sdkModule = module
}

func kwOrEmpty(kw map[string]any) map[string]any {
	if kw == nil {
		return map[string]any{}
	}
	return kw
}

// Remembers an object under a new id owned by a plugin key
func (rt *runtime) remember(p py, owner string, v Ref) int64 {
	kept := p.keep(v)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.nextID++
	rt.refs[rt.nextID] = kept
	rt.owners[rt.nextID] = owner
	return rt.nextID
}

func (rt *runtime) lookup(id int64) (Ref, string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.refs[id], rt.owners[id]
}

func (rt *runtime) pop(id int64) Ref {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	v := rt.refs[id]
	delete(rt.refs, id)
	delete(rt.owners, id)
	return v
}

// Whether a value is a plugin defined instance as opposed to a function, method, partial or class
func isProviderObject(p py, v Ref) bool {
	types := p.imp("types")
	kinds := p.call(p.attr("builtins", "tuple"), []any{p.get(types, "FunctionType"), p.get(types, "MethodType"),
		p.get(types, "BuiltinFunctionType"), p.attr("functools", "partial"), p.attr("builtins", "type")})
	if p.isinstance(v, kinds) {
		return false
	}
	return p.str(p.get(p.typeOf(v), "__module__")) != "builtins"
}

// Reference dict for a callable, event or provider object, nil for plain data
func (rt *runtime) refFor(p py, owner string, v Ref, allowObjects bool) map[string]any {
	if p.isType(v) {
		return nil
	}
	if p.isinstance(v, p.attr("threading", "Event")) {
		return rt.eventRef(p, v, owner)
	}
	if p.callable(v) && !(allowObjects && isProviderObject(p, v)) {
		return rt.callableRef(p, owner, v)
	}
	if allowObjects && isProviderObject(p, v) {
		return rt.describeObject(p, owner, v)
	}
	return nil
}

// Reference dict of a callable the parent calls back by id
func (rt *runtime) callableRef(p py, owner string, v Ref) map[string]any {
	name := "callback"
	if n := p.getOr(v, "__name__"); n != nil {
		name = p.str(n)
	}
	return map[string]any{"__callable__": rt.remember(p, owner, v), "async": isAsyncCallable(p, v),
		"sig": describeSignature(p, v), "name": name, "qualname": optionalAttr(p, v, "__qualname__"),
		"module": optionalAttr(p, v, "__module__")}
}

// Reference of an argument an in process registrar receives, classes and callables by id and instances as objects judged against base
func (rt *runtime) inProcessRef(p py, owner string, v Ref, base string) map[string]any {
	switch {
	case p.isinstance(v, p.attr("threading", "Event")):
		return rt.eventRef(p, v, owner)
	case p.isType(v) || p.callable(v):
		return rt.callableRef(p, owner, v)
	case isProviderObject(p, v):
		ref := rt.describeObject(p, owner, v)
		if base != "" {
			ref["identity"] = map[string]any{"is_base": isBaseInstance(p, v, base)}
		}
		return ref
	}
	return nil
}

// Whether a value is an instance of the class a module:attr reference names, false when that class cannot be imported
func isBaseInstance(p py, v Ref, base string) bool {
	result := false
	if err := p.attempt(func() { result = p.isinstance(v, importBase(p, base)) }, "ImportError", "AttributeError"); err != nil {
		return false
	}
	return result
}

func optionalAttr(p py, v Ref, name string) any {
	a := p.getOr(v, name)
	if a == nil {
		return nil
	}
	return p.val(a)
}

// Method table and live attribute plan of a provider object whose methods all run in the host
func (rt *runtime) describeObject(p py, owner string, obj Ref) map[string]any {
	methods := map[string]any{}
	instance := []string{}
	if p.has(obj, "__dict__") {
		for _, k := range p.items(p.call(p.attr("builtins", "vars"), obj)) {
			if name := p.str(k); !strings.HasPrefix(name, "_") {
				instance = append(instance, name)
			}
		}
		sort.Strings(instance)
	}
	live := append([]string{}, instance...)
	isInstance := map[string]bool{}
	for _, n := range instance {
		isInstance[n] = true
	}
	getattrStatic := p.attr("inspect", "getattr_static")
	property := p.attr("builtins", "property")
	for _, name := range p.dir(obj) {
		if strings.HasPrefix(name, "_") || isInstance[name] {
			continue
		}
		raw := p.call(getattrStatic, obj, name, nil)
		if p.isinstance(raw, property) || p.typeName(raw) == "cached_property" {
			live = append(live, name)
			continue
		}
		value := p.getOr(obj, name)
		if value != nil && p.callable(value) {
			methods[name] = map[string]any{"async": isAsyncCallable(p, value), "sig": describeSignature(p, value)}
		} else {
			live = append(live, name)
		}
	}
	return map[string]any{"__object__": rt.remember(p, owner, obj), "methods": methods, "live": anyStrings(live), "type": p.typeName(obj)}
}

// Method table of a module or class whose functions run in the host, every other name read live
func (rt *runtime) describeNamespace(p py, owner string, obj Ref) map[string]any {
	methods := map[string]any{}
	live := []string{}
	for _, name := range p.dir(obj) {
		if strings.HasPrefix(name, "_") {
			continue
		}
		value := p.getOr(obj, name)
		if value != nil && p.callable(value) && !p.isType(value) {
			methods[name] = map[string]any{"async": isAsyncCallable(p, value), "sig": describeSignature(p, value)}
		} else {
			live = append(live, name)
		}
	}
	return map[string]any{"__object__": rt.remember(p, owner, obj), "methods": methods, "live": anyStrings(live), "type": p.typeName(obj)}
}

func anyStrings(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// Wire ref of a threading Event, a parent event keeping its id and a host event remembered here
func (rt *runtime) eventRef(p py, v Ref, owner string) map[string]any {
	if !p.isinstance(v, p.attr("threading", "Event")) {
		return nil
	}
	if p.isinstance(v, rt.parentEventClass) {
		return p.val(p.m(v, "__icebath_ref__")).(map[string]any)
	}
	return map[string]any{"__child_event__": rt.remember(p, owner, v)}
}

func (rt *runtime) resolveEvent(p py, ref map[string]any) Ref {
	if id, ok := ref["__parent_event__"]; ok {
		return rt.newParentEvent(p, toInt(id))
	}
	id := toInt(ref["__child_event__"])
	event, _ := rt.lookup(id)
	if event == nil || !p.isinstance(event, p.attr("threading", "Event")) {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host event %d was released", id)})
	}
	return event
}

// Resolves refs in values the parent answers, unknown kinds becoming opaque stand ins
func (rt *runtime) resolveFromParent(p py, ref map[string]any) Ref {
	if _, ok := ref["__parent_event__"]; ok {
		return rt.resolveEvent(p, ref)
	}
	if _, ok := ref["__child_event__"]; ok {
		return rt.resolveEvent(p, ref)
	}
	if id, ok := ref["__parent_callable__"]; ok {
		return rt.newParentCallable(p, toInt(id), "")
	}
	if id, ok := ref["__parent_object__"]; ok {
		return rt.newParentObject(p, toInt(id), str(ref["type"]))
	}
	if id, ok := ref["__handle__"]; ok {
		return rt.newRegistration(p, toInt(id), str(ref["kind"]), str(ref["key"]))
	}
	kind := "object"
	if _, ok := ref["__callable__"]; ok {
		kind = "callable"
	}
	return rt.newOpaque(p, kind, "")
}

// Resolve function for SDK answers, events resolved and other ref dicts decoded as plain dicts
func (rt *runtime) resolveEventsOnly(p py, ref map[string]any) Ref {
	if _, ok := ref["__parent_event__"]; ok {
		return rt.resolveEvent(p, ref)
	}
	if _, ok := ref["__child_event__"]; ok {
		return rt.resolveEvent(p, ref)
	}
	out := map[string]any{}
	for k, v := range ref {
		out[k] = rt.decodeValue(p, v, rt.resolveEventsOnly)
	}
	return p.obj(out)
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

// Queues a parent callable id the host no longer holds
func (rt *runtime) releaseParent(id int64) {
	rt.mu.Lock()
	rt.released = append(rt.released, id)
	rt.mu.Unlock()
}

// Tells the parent which of its callables the host dropped
func (rt *runtime) flushParentReleases(ctx context.Context) error {
	rt.mu.Lock()
	ids := rt.released
	rt.released = nil
	rt.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	list := make([]any, len(ids))
	for i, id := range ids {
		list[i] = id
	}
	_, err := rt.ch.Call(ctx, "release_parent", map[string]any{"ids": list}, 0)
	var unavailable *plugins.PluginHostUnavailable
	if errors.As(err, &unavailable) {
		return nil
	}
	return err
}

// Sends a request for plugin code running on this thread, the GIL released while the parent answers
func (rt *runtime) send(p py, method string, params map[string]any) any {
	origin := p.val(p.m(rt.serving, "get"))
	pyctx := p.call(p.attr("contextvars", "copy_context"))
	ctx := withPyContext(plugins.WithServingRequest(context.Background(), origin), pyctx)
	var result any
	var err error
	rt.blocking(p.g, func() {
		if err = rt.flushParentReleases(ctx); err == nil {
			result, err = rt.ch.Call(ctx, method, params, 0)
		}
	})
	if err != nil {
		throw(rt.remoteError(p, err))
	}
	return result
}

// Runs fn on another goroutine while this thread waits with the GIL released
func (rt *runtime) blocking(g *pyhost.GIL, fn func()) {
	g.Unblocked(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			fn()
		}()
		<-done
	})
}

// Python exception for a parent error, builtins plugins catch raised as themselves
func (rt *runtime) remoteError(p py, err error) error {
	var unavailable *plugins.PluginHostUnavailable
	if errors.As(err, &unavailable) {
		return p.g.Raising(p.call(rt.unavailableClass, unavailable.Reason))
	}
	var unsupported *plugins.PluginHostUnsupported
	if errors.As(err, &unsupported) {
		return p.g.Raising(p.call(rt.unsupportedClass, unsupported.Msg))
	}
	var builtin *plugins.HostBuiltinError
	if errors.As(err, &builtin) {
		return &pyhost.Exception{Type: builtin.Type, Message: builtin.Message}
	}
	var remote *plugins.PluginHostError
	if errors.As(err, &remote) {
		if module, ok := sdkExceptions[remote.TypeName]; ok {
			return &pyhost.Exception{Type: module + "." + remote.TypeName, Message: remote.RemoteMessage}
		}
		return p.g.Raising(p.call(rt.hostErrorClass, remote.TypeName, remote.RemoteMessage))
	}
	var interrupted *plugins.UserInterrupt
	if errors.As(err, &interrupted) {
		return &pyhost.Exception{Type: "KeyboardInterrupt", Message: interrupted.Error()}
	}
	return err
}

// Facade errors raised as the SDK class plugins import, keyed by the remote class name
var sdkExceptions = map[string]string{"PluginLlmTrustError": "agent.plugin_llm", "SubagentLifecycleError": "agent.subagent_lifecycle",
	"DelegationRouteError": "agent.delegation_provider"}

// Sends a ctx or facade call with arguments encoded under the plugin key
func (rt *runtime) sendCall(p py, method, pluginKey string, payload map[string]any, args []any, kwargs map[string]any, refs refsFunc) Ref {
	payload["plugin"] = pluginKey
	payload["args"] = rt.encode(p, p.obj(args), refs)
	payload["kwargs"] = rt.encode(p, p.obj(kwOrEmpty(kwargs)), refs)
	return rt.decode(p, rt.send(p, method, payload), rt.resolveFromParent)
}

// Forwards one ctx method call, provider objects crossing by reference for the registrars that take them
func (rt *runtime) ctxCall(p py, pluginKey, method string, args []any, kwargs map[string]any) Ref {
	if method == "register_login_backend" {
		return rt.sendCall(p, "ctx", pluginKey, map[string]any{"method": method}, args, kwargs,
			func(p py, o Ref) map[string]any { return rt.judgedRef(p, pluginKey, o) })
	}
	_, unsupported := plugins.HostUnsupportedCtxMethods[method]
	if _, skipped := plugins.HostSkippedCtxMethods[method]; unsupported || skipped {
		return rt.sendCall(p, "ctx", pluginKey, map[string]any{"method": method}, args, kwargs,
			func(p py, o Ref) map[string]any {
				return rt.inProcessRef(p, pluginKey, o, plugins.HostInProcessObjectBases[method])
			})
	}
	_, allow := plugins.HostObjectBases[method]
	return rt.sendCall(p, "ctx", pluginKey, map[string]any{"method": method}, args, kwargs,
		func(p py, o Ref) map[string]any { return rt.refFor(p, pluginKey, o, allow) })
}

// Unread ref of a plugin object carrying its statically judged login backend identity
func (rt *runtime) judgedRef(p py, pluginKey string, v Ref) map[string]any {
	if p.isType(v) || p.isinstance(v, p.attr("threading", "Event")) || !isProviderObject(p, v) {
		return rt.refFor(p, pluginKey, v, false)
	}
	return map[string]any{"__object__": rt.remember(p, pluginKey, v), "methods": map[string]any{}, "live": []any{},
		"type": p.typeName(v), "identity": loginBackendIdentity(p, v)}
}

// Runs one facade method on the parent side object
func (rt *runtime) facadeCall(p py, pluginKey, facade, method string, probe bool, args []any, kwargs map[string]any) Ref {
	return rt.sendCall(p, "facade", pluginKey, map[string]any{"facade": facade, "method": method, "probe": probe}, args, kwargs,
		func(p py, o Ref) map[string]any { return rt.refFor(p, pluginKey, o, false) })
}

// Schedules a coroutine on the host loop as a task bound to the request being served, answered as a concurrent future
func (rt *runtime) spawn(p py, coro Ref) Ref {
	if !p.truthy(p.m(p.imp("inspect"), "iscoroutine", coro)) {
		throw(&pyhost.Exception{Type: "TypeError", Message: "spawn_task() requires a coroutine object"})
	}
	kept := []Ref{p.keep(coro), p.keep(p.m(rt.serving, "get")), p.keep(rt.loop(p)), p.keep(p.call(p.attr("concurrent.futures", "Future")))}
	coroK, origin, loop, future := kept[0], kept[1], kept[2], kept[3]
	release := func() {
		for _, r := range kept {
			r.Release()
		}
	}
	start := p.fn("_on_behalf_of", func(p py, _ []any, _ map[string]any) any {
		defer release()
		err := p.attempt(func() {
			taskCtx := p.m(p.imp("contextvars"), "copy_context")
			p.m(taskCtx, "run", p.get(rt.serving, "set"), origin)
			task := p.mKw(loop, "create_task", []any{coroK}, map[string]any{"context": taskCtx})
			p.call(p.attr("asyncio.futures", "_chain_future"), task, future)
		}, "Exception")
		if err != nil {
			if p.truthy(p.m(future, "set_running_or_notify_cancel")) {
				p.m(future, "set_exception", err.(*pyhost.PyError).Exception())
			}
			throw(err)
		}
		return nil
	})
	result := p.obj(future)
	if err := p.attempt(func() { p.m(loop, "call_soon_threadsafe", start) }, "BaseException"); err != nil {
		release()
		throw(err)
	}
	return result
}

// Event loop of the host thread every plugin coroutine runs on
func (rt *runtime) loop(p py) Ref { return p.g.NewRef(p.g.Loop()) }

// Logs a warning through the child logger
func (rt *runtime) warn(p py, format string, args ...any) {
	p.m(rt.logger, "warning", append([]any{format}, args...)...)
}

// Ends serving and lets the entry finalize the interpreter
func (rt *runtime) stop() { rt.stopOnce.Do(func() { close(rt.stopped) }) }
