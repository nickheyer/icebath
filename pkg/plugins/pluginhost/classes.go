package pluginhost

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Class method body run in its own reference scope, self first and the result converted before the scope ends
func method(body func(p py, self Ref, args []any, kw map[string]any) any) pyhost.RawFunc {
	return func(g *pyhost.GIL, args, kwargs uintptr) (out uintptr, err error) {
		var scope []Ref
		defer func() {
			for _, r := range scope {
				r.Release()
			}
			g.Drain()
		}()
		defer catch(&err)
		p := py{g: g, scope: &scope}
		items := p.items(p.track(g.NewRef(args)))
		if len(items) == 0 {
			return 0, &pyhost.Exception{Type: "TypeError", Message: "method called without self"}
		}
		rest := make([]any, 0, len(items)-1)
		for _, item := range items[1:] {
			rest = append(rest, p.arg(item))
		}
		return g.ToPython(body(p, items[0], rest, p.kwargs(kwargs)))
	}
}

// Wire helper classes, the host error classes and the parent side objects plugin code holds
func (rt *runtime) setupClasses(p py) {
	wire := "icebath_cli.plugin_host_wire"
	builtins := p.imp("builtins")
	rt.recordClass = rt.class(p, wire, "Record", []any{p.get(builtins, "dict")}, map[string]any{
		"__getattr__": method(func(p py, self Ref, args []any, _ map[string]any) any {
			name := p.text(args[0])
			if !p.truthy(p.m(self, "__contains__", name)) {
				throw(&pyhost.Exception{Type: "AttributeError", Message: name})
			}
			return p.item(self, name)
		}),
	})
	rt.opaqueClass = rt.class(p, wire, "Opaque", nil, map[string]any{
		"__init__": method(func(p py, self Ref, args []any, _ map[string]any) any {
			text := ""
			if len(args) > 1 {
				text = p.text(args[1])
			}
			p.set(self, "type_name", args[0])
			p.set(self, "text", text)
			return nil
		}),
		"__getattr__": method(func(p py, self Ref, args []any, _ map[string]any) any {
			typeName := p.repr(p.m(self, "__getattribute__", "type_name"))
			throw(&pyhost.Exception{Type: "AttributeError", Message: fmt.Sprintf("%s is a live object in the Icebath process and is not available inside the plugin host (attribute %s)", typeName, p.repr(p.obj(args[0])))})
			return nil
		}),
		"__bool__": method(func(py, Ref, []any, map[string]any) any { return true }),
		"__repr__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			return "<opaque " + p.str(p.m(self, "__getattribute__", "type_name")) + ">"
		}),
	})
	runtimeError := p.get(builtins, "RuntimeError")
	rt.hostErrorClass = rt.class(p, wire, "PluginHostError", []any{runtimeError}, map[string]any{
		"__init__": method(func(p py, self Ref, args []any, _ map[string]any) any {
			typeName, message := p.text(args[0]), p.text(args[1])
			text := message
			if typeName != "" {
				text = typeName + ": " + message
			}
			p.m(p.attr("builtins", "RuntimeError"), "__init__", self, text)
			p.set(self, "type_name", typeName)
			p.set(self, "remote_message", message)
			return nil
		}),
	})
	rt.unavailableClass = rt.class(p, wire, "PluginHostUnavailable", []any{runtimeError}, nil)
	rt.unsupportedClass = rt.class(p, wire, "PluginHostUnsupported", []any{runtimeError}, nil)

	child := "icebath_cli.plugin_host_child"
	idOf := func(p py, self Ref) int64 { return toInt(p.val(p.get(self, "_id"))) }
	rt.parentCallClass = rt.class(p, child, "ParentCallable", nil, map[string]any{
		"__call__": method(func(p py, self Ref, args []any, kw map[string]any) any {
			return rt.parentCall(p, idOf(p, self), p.text(p.get(self, "_owner")), args, kw)
		}),
		"__repr__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			return fmt.Sprintf("<icebath callable %d>", idOf(p, self))
		}),
		"__del__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			if id := p.getOr(self, "_id"); id != nil {
				rt.releaseParent(toInt(p.val(id)))
			}
			return nil
		}),
	})
	eventOp := func(op string) pyhost.RawFunc {
		return method(func(p py, self Ref, args []any, kw map[string]any) any {
			var timeout any
			if len(args) > 0 {
				timeout = args[0]
			} else if t, ok := kw["timeout"]; ok {
				timeout = t
			}
			v := rt.parentEventOp(p, idOf(p, self), op, timeout)
			if op == "is_set" || op == "wait" {
				return p.truthy(p.obj(v))
			}
			return nil
		})
	}
	rt.parentEventClass = rt.class(p, child, "ParentEvent", []any{p.attr("threading", "Event")}, map[string]any{
		"__icebath_ref__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			return map[string]any{"__parent_event__": idOf(p, self)}
		}),
		"is_set": eventOp("is_set"), "isSet": eventOp("is_set"), "set": eventOp("set"), "clear": eventOp("clear"), "wait": eventOp("wait"),
		"__repr__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			state := "unset"
			if p.truthy(p.m(self, "is_set")) {
				state = "set"
			}
			return fmt.Sprintf("<icebath event %d: %s>", idOf(p, self), state)
		}),
	})
	rt.registrationCls = rt.class(p, child, "RemoteRegistration", nil, map[string]any{
		"active": pyhost.Property(func(g *pyhost.GIL, self uintptr) (out any, err error) {
			return runValue(g, func(p py) any { return !p.truthy(p.get(p.track(g.NewRef(self)), "_disposed")) })
		}),
		"dispose": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			if p.truthy(p.get(self, "_disposed")) {
				return nil
			}
			p.set(self, "_disposed", true)
			rt.send(p, "dispose", map[string]any{"handle": idOf(p, self)})
			return nil
		}),
	})
}

// Runs a body producing a plain Go value in a reference scope
func runValue(g *pyhost.GIL, body func(p py) any) (out any, err error) {
	err = run(g, func(p py) { out = body(p) })
	return out, err
}

// NoopPluginContext subclass keeping the one register_computer_use_provider argument
func (rt *runtime) computerUseCollector(p py) Ref {
	cls := rt.class(p, "icebath_cli.plugin_host_child", "_ComputerUseProviderCollector", []any{p.attr("plugins.plugin_loader", "NoopPluginContext")}, map[string]any{
		"__init__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			p.set(self, "provider", nil)
			return nil
		}),
		"register_computer_use_provider": method(func(p py, self Ref, args []any, kw map[string]any) any {
			v := kw["provider"]
			if len(args) > 0 {
				v = args[0]
			}
			p.set(self, "provider", v)
			return nil
		}),
	})
	return p.call(cls)
}

// Coroutine function stand in running a facade call on a worker thread, marked for inspect and asyncio
func (rt *runtime) asyncFacadeMethod(p py, name string, call Ref) Ref {
	rt.mu.Lock()
	cls := rt.asyncMethodClass
	rt.mu.Unlock()
	if cls == nil {
		cls = rt.class(p, "icebath_cli.plugin_host_child", "remote", nil, map[string]any{
			"__call__": method(func(p py, self Ref, args []any, kw map[string]any) any {
				return p.callKw(p.attr("asyncio", "to_thread"), append([]any{p.get(self, "_call")}, args...), kw)
			}),
		})
		rt.mu.Lock()
		rt.asyncMethodClass = p.keep(cls)
		rt.mu.Unlock()
	}
	obj := rt.bare(p, cls)
	p.set(obj, "_call", call)
	p.set(obj, "__name__", name)
	if mark := p.getOr(p.imp("inspect"), "markcoroutinefunction"); mark != nil {
		p.call(mark, obj)
	}
	if marker := p.getOr(p.imp("asyncio.coroutines"), "_is_coroutine"); marker != nil {
		p.set(obj, "_is_coroutine", marker)
	}
	return obj
}

// Instance of the class standing for a parent object of one Python type, its attributes read from the parent
func (rt *runtime) newParentObject(p py, id int64, typeName string) Ref {
	if typeName == "" {
		typeName = "object"
	}
	rt.mu.Lock()
	cls := rt.parentObjClasses[typeName]
	rt.mu.Unlock()
	if cls == nil {
		cls = rt.parentObjectClass(p, typeName)
	}
	obj := rt.bare(p, cls)
	p.set(obj, "_id", id)
	return obj
}

// Class whose missing attributes, repr and finalizer run against the parent object a ctx method returned
func (rt *runtime) parentObjectClass(p py, typeName string) Ref {
	idOf := func(p py, self Ref) int64 { return toInt(p.val(p.m(self, "__getattribute__", "_id"))) }
	cls := rt.class(p, "icebath_cli.plugin_host_child", typeName, nil, map[string]any{
		"__getattr__": method(func(p py, self Ref, args []any, _ map[string]any) any {
			name := pyStr(args[0])
			if strings.HasPrefix(name, "_") {
				throw(&pyhost.Exception{Type: "AttributeError", Message: name})
			}
			id := idOf(p, self)
			value := rt.decode(p, rt.send(p, "parent_object", map[string]any{"id": id, "method": name, "probe": true}), rt.resolveFromParent)
			if !p.isinstance(value, p.attr("builtins", "dict")) || !p.truthy(p.m(value, "get", "__method__")) {
				return value
			}
			call := p.fn(name, func(p py, args []any, kw map[string]any) any {
				refs := func(p py, o Ref) map[string]any { return rt.refFor(p, "", o, false) }
				return rt.decode(p, rt.send(p, "parent_object", map[string]any{"id": id, "method": name, "probe": false,
					"args": rt.encode(p, p.obj(args), refs), "kwargs": rt.encode(p, p.obj(kwOrEmpty(kw)), refs)}), rt.resolveFromParent)
			})
			if p.truthy(p.m(value, "get", "async")) {
				return rt.asyncFacadeMethod(p, name, call)
			}
			return call
		}),
		"__repr__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			return rt.send(p, "parent_object", map[string]any{"id": idOf(p, self), "repr": true})
		}),
		"__del__": method(func(p py, self Ref, _ []any, _ map[string]any) any {
			if id := p.getOr(self, "_id"); id != nil {
				rt.releaseParent(toInt(p.val(id)))
			}
			return nil
		}),
	})
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.parentObjClasses == nil {
		rt.parentObjClasses = map[string]Ref{}
	}
	if existing := rt.parentObjClasses[typeName]; existing != nil {
		return existing
	}
	rt.parentObjClasses[typeName] = p.keep(cls)
	return rt.parentObjClasses[typeName]
}
