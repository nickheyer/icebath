package pluginhost

import (
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Instance of a helper class without running an initializer
func (rt *runtime) bare(p py, cls Ref) Ref { return p.m(cls, "__new__", cls) }

func (rt *runtime) newParentCallable(p py, id int64, owner string) Ref {
	obj := rt.bare(p, rt.parentCallClass)
	p.set(obj, "_id", id)
	p.set(obj, "_owner", owner)
	return obj
}

func (rt *runtime) newParentEvent(p py, id int64) Ref {
	obj := rt.bare(p, rt.parentEventClass)
	p.m(p.attr("threading", "Event"), "__init__", obj)
	p.set(obj, "_id", id)
	return obj
}

func (rt *runtime) newRegistration(p py, id int64, kind, key string) Ref {
	obj := rt.bare(p, rt.registrationCls)
	p.set(obj, "_id", id)
	p.set(obj, "kind", kind)
	p.set(obj, "key", key)
	p.set(obj, "_disposed", false)
	return obj
}

// Runs a parent callable and returns its decoded answer
func (rt *runtime) parentCall(p py, id int64, owner string, args []any, kwargs map[string]any) Ref {
	refs := func(p py, o Ref) map[string]any { return rt.refFor(p, owner, o, false) }
	payload := map[string]any{"id": id, "args": rt.encode(p, p.obj(args), refs), "kwargs": rt.encode(p, p.obj(kwOrEmpty(kwargs)), refs)}
	return rt.decode(p, rt.send(p, "parent_call", payload), rt.resolveFromParent)
}

// One operation on a parent event, a closed channel reading as set so waiting loops stop
func (rt *runtime) parentEventOp(p py, id int64, op string, timeout any) any {
	var out any
	err := p.attempt(func() {
		out = rt.send(p, "parent_event", map[string]any{"id": id, "op": op, "timeout": timeout})
	}, "icebath_cli.plugin_host_wire.PluginHostUnavailable")
	if err != nil {
		return op == "is_set" || op == "wait"
	}
	return out
}

type ctxMode int

const (
	ctxRemote ctxMode = iota
	ctxCapturing
	ctxCollector
	ctxProviderCollector
)

// Go side context a plugin's register receives, mirroring the parent's public surface
type pluginCtx struct {
	rt        *runtime
	key       string
	methods   map[string]bool
	inProcess map[string]bool
	info      map[string]any

	mode     ctxMode
	capture  string
	base     Ref
	forward  bool
	class    string
	mu       sync.Mutex
	captured Ref
	facades  map[string]*facade
	manifest Ref
}

func newPluginCtx(p py, rt *runtime, key string, info map[string]any) *pluginCtx {
	c := &pluginCtx{rt: rt, key: key, info: info, methods: stringSet(info["ctx_methods"]), inProcess: stringSet(info["in_process_ctx_methods"]),
		facades: map[string]*facade{}}
	manifest, _ := info["manifest"].(map[string]any)
	c.manifest = p.keep(p.callKw(p.attr("types", "SimpleNamespace"), nil, kwOrEmpty(manifest)))
	return c
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if list, ok := v.([]any); ok {
		for _, item := range list {
			out[str(item)] = true
		}
	}
	return out
}

func (c *pluginCtx) TypeName() (string, string) {
	switch c.mode {
	case ctxCapturing:
		return "icebath_cli.plugin_host_child", "_CapturingContext"
	case ctxCollector:
		return "icebath_cli.plugin_host_child", "_CollectorContext"
	case ctxProviderCollector:
		return "icebath_cli.plugin_host_child", "_ProviderCollector"
	}
	return "icebath_cli.plugin_host_child", "RemotePluginContext"
}

func (c *pluginCtx) GetAttr(g *pyhost.GIL, name string) (out any, ok bool, err error) {
	defer catch(&err)
	p := py{g: g}
	switch name {
	case "manifest":
		return c.manifest, true, nil
	case "plugin_id":
		if id, has := c.info["plugin_id"]; has && id != nil && id != "" {
			return id, true, nil
		}
		return c.key, true, nil
	case "profile_name":
		return c.info["profile_name"], true, nil
	case "current_cron_execution":
		return p.fn(name, c.currentCronExecution), true, nil
	case "on_unload":
		return p.fn(name, c.onUnload), true, nil
	case "spawn_task":
		return p.fn(name, func(p py, args []any, _ map[string]any) any {
			if len(args) != 1 {
				throw(&pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("spawn_task() takes 2 positional arguments but %d were given", len(args)+1)})
			}
			return c.rt.spawn(p, p.obj(args[0]))
		}), true, nil
	}
	switch c.mode {
	case ctxCapturing:
		if name == c.capture {
			return p.fn("capture", c.captureFirst), true, nil
		}
		if !c.forward && (strings.HasPrefix(name, "register_") || name == "subscribe") {
			return p.fn("<lambda>", func(py, []any, map[string]any) any { return nil }), true, nil
		}
	case ctxCollector:
		switch name {
		case "collected":
			return c.collectedValue(), true, nil
		case c.capture:
			return p.fn("capture", c.captureLast), true, nil
		}
		if strings.HasPrefix(name, "_") || !c.methods[name] {
			return nil, false, &pyhost.Exception{Type: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute %s", c.class, p.repr(p.obj(name)))}
		}
		return c.forwarder(p, name), true, nil
	case ctxProviderCollector:
		switch name {
		case "provider":
			return c.collectedValue(), true, nil
		case "register_memory_provider":
			return p.fn(name, c.captureLast), true, nil
		case "collect":
			return p.fn(name, c.collect), true, nil
		}
	}
	return c.remoteAttr(p, name)
}

// Attribute lookup of RemotePluginContext for names its instance and class do not define
func (c *pluginCtx) remoteAttr(p py, name string) (any, bool, error) {
	if strings.HasPrefix(name, "_") {
		return nil, false, &pyhost.Exception{Type: "AttributeError", Message: name}
	}
	if reason, refused := plugins.HostUnsupportedCtxMethods[name]; refused && !c.inProcess[name] {
		return p.fn("unsupported", func(p py, _ []any, _ map[string]any) any {
			throw(p.g.Raising(p.call(c.rt.unsupportedClass, fmt.Sprintf("ctx.%s() cannot run in the plugin host: %s", name, reason))))
			return nil
		}), true, nil
	}
	if reason, skipped := plugins.HostSkippedCtxMethods[name]; skipped && !c.inProcess[name] {
		return p.fn("skipped", func(p py, _ []any, _ map[string]any) any {
			c.rt.warn(p, "Plugin '%s': ctx.%s() is skipped in the plugin host (%s)", p.get(c.manifest, "name"), name, reason)
			return nil
		}), true, nil
	}
	if plugins.HostRemoteFacades[name] {
		c.mu.Lock()
		f := c.facades[name]
		if f == nil {
			f = &facade{ctx: c, name: name, methods: map[string]bool{}}
			c.facades[name] = f
		}
		c.mu.Unlock()
		return f, true, nil
	}
	if !c.methods[name] {
		return nil, false, &pyhost.Exception{Type: "AttributeError", Message: fmt.Sprintf("'PluginContext' object has no attribute %s", p.repr(p.obj(name)))}
	}
	if name == "apost_message" {
		return c.rt.asyncFacadeMethod(p, name, c.forwarder(p, name)), true, nil
	}
	return c.forwarder(p, name), true, nil
}

// Native function forwarding one ctx method to the parent under this plugin key
func (c *pluginCtx) forwarder(p py, name string) Ref {
	return p.fn(name, func(p py, args []any, kw map[string]any) any { return c.rt.ctxCall(p, c.key, name, args, kw) })
}

// The CronExecution the calling code runs inside, None outside a fire
func (c *pluginCtx) currentCronExecution(p py, _ []any, _ map[string]any) any {
	if !c.methods["current_cron_execution"] {
		throw(&pyhost.Exception{Type: "AttributeError", Message: "'PluginContext' object has no attribute 'current_cron_execution'"})
	}
	value := c.rt.ctxCall(p, c.key, "current_cron_execution", nil, nil)
	return c.rt.restore(p, value, p.attr("cron.execution_identity", "CronExecution"))
}

func (c *pluginCtx) onUnload(p py, args []any, kw map[string]any) any {
	cb := kw["callback"]
	if len(args) > 0 {
		cb = args[0]
	}
	ref := p.obj(cb)
	if !p.callable(ref) {
		throw(&pyhost.Exception{Type: "TypeError", Message: "on_unload() callback must be callable"})
	}
	kept := p.keep(ref)
	c.rt.mu.Lock()
	if _, ok := c.rt.unloadCallbacks[c.key]; !ok {
		c.rt.unloadOrder = append(c.rt.unloadOrder, c.key)
	}
	c.rt.unloadCallbacks[c.key] = append(c.rt.unloadCallbacks[c.key], kept)
	c.rt.mu.Unlock()
	return nil
}

// Keeps the first registered object that is an instance of the category base
func (c *pluginCtx) captureFirst(p py, args []any, kw map[string]any) any {
	v, ok := kw["obj"]
	if len(args) > 0 {
		v, ok = args[0], true
	}
	if !ok {
		throw(&pyhost.Exception{Type: "TypeError", Message: "capture() missing 1 required positional argument: 'obj'"})
	}
	obj := p.obj(v)
	if !p.isinstance(obj, c.base) {
		return nil
	}
	kept := p.keep(obj)
	c.mu.Lock()
	if c.captured == nil {
		c.captured = kept
	}
	c.mu.Unlock()
	return nil
}

// Keeps the last registered object
func (c *pluginCtx) captureLast(p py, args []any, kw map[string]any) any {
	if len(args)+len(kw) != 1 {
		throw(&pyhost.Exception{Type: "TypeError", Message: fmt.Sprintf("capture() takes 1 positional argument but %d were given", len(args)+len(kw))})
	}
	var v any
	if len(args) > 0 {
		v = args[0]
	}
	for _, k := range sortedKeys(kw) {
		v = kw[k]
	}
	kept := p.keep(p.obj(v))
	c.mu.Lock()
	c.captured = kept
	c.mu.Unlock()
	return nil
}

// The collected value, nil when nothing registered it
func (c *pluginCtx) collectedValue() Ref {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captured
}

// Drops the parent's fallback hooks of the register function's source file, then runs register on this collector
func (c *pluginCtx) collect(p py, args []any, kw map[string]any) any {
	if len(args) == 0 {
		throw(&pyhost.Exception{Type: "TypeError", Message: "collect() missing 1 required positional argument: 'register'"})
	}
	register := p.obj(args[0])
	source := kw["source"]
	if s, isStr := source.(string); source == nil || isStr && s == "" {
		source = nil
		modName := ""
		if m := p.getOr(register, "__module__"); m != nil && !p.isNone(m) {
			modName = p.str(m)
		}
		if module := p.m(p.attr("sys", "modules"), "get", modName); !p.isNone(module) {
			if f := p.getOr(module, "__file__"); f != nil {
				source = p.val(f)
			}
		}
	}
	c.rt.send(p, "collect_source", map[string]any{"plugin": c.key, "source": source})
	p.call(register, c.rt.ctxObject(p, c))
	return nil
}

// Parent side facade, every method runs on the parent object and plain attributes are read fresh
type facade struct {
	ctx     *pluginCtx
	name    string
	mu      sync.Mutex
	methods map[string]bool
}

func (f *facade) TypeName() (string, string) { return "icebath_cli.plugin_host_child", "RemoteFacade" }

func (f *facade) GetAttr(g *pyhost.GIL, attr string) (out any, ok bool, err error) {
	defer catch(&err)
	p := py{g: g}
	if strings.HasPrefix(attr, "_") {
		return nil, false, &pyhost.Exception{Type: "AttributeError", Message: attr}
	}
	f.mu.Lock()
	async, known := f.methods[attr]
	f.mu.Unlock()
	if !known {
		value := f.ctx.rt.facadeCall(p, f.ctx.key, f.name, attr, true, nil, nil)
		if !p.isinstance(value, p.attr("builtins", "dict")) || !p.truthy(p.m(value, "get", "__method__")) {
			return value, true, nil
		}
		async = p.truthy(p.m(value, "get", "async"))
		f.mu.Lock()
		f.methods[attr] = async
		f.mu.Unlock()
	}
	call := p.fn(attr, func(p py, args []any, kw map[string]any) any {
		return f.ctx.rt.facadeCall(p, f.ctx.key, f.name, attr, false, args, kw)
	})
	if !async {
		return call, true, nil
	}
	return f.ctx.rt.asyncFacadeMethod(p, attr, call), true, nil
}
