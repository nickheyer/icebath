package pluginhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const (
	entryPointsGroup       = "icebath_agent.plugins"
	memoryEntryPointsGroup = "icebath_agent.memory_providers"
	userMemoryNamespace    = "_icebath_user_memory"
	entryPointProfileKey   = "entrypoint:"
	profileClientOwner     = "profile-client:"
	providerModuleOwner    = "provider-module:"
)

type opFunc func(rt *runtime, p py, ctx context.Context, params map[string]any) any

var opTable map[string]opFunc

func init() {
	opTable = map[string]opFunc{
		"hello": opHello, "load": opLoad, "load_tools": opLoadTools, "load_instance": opLoadInstance,
		"load_computer_use_provider": opLoadComputerUseProvider, "asgi": opASGI, "profile_call": opProfileCall,
		"profile_client": opProfileClient, "provider_entry_point_load": opProviderEntryPointLoad,
		"provider_entry_point_invoke": opProviderEntryPointInvoke, "provider_module_call": opProviderModuleCall,
		"provider_module_import": opProviderModuleImport, "provider_module_json_call": opProviderModuleJSONCall,
		"provider_module_hold": opProviderModuleHold, "provider_module_release": opProviderModuleRelease,
		"invoke": opInvoke, "obj_invoke": opObjInvoke, "obj_getattr": opObjGetattr, "obj_next": opObjNext,
		"obj_overrides": opObjOverrides, "login_backend": opLoginBackend, "obj_setattr": opObjSetattr,
		"unload": opUnload, "evict_modules": opEvictModules, "release": opRelease, "event": opEvent,
		"config_schema": opConfigSchema, "load_hook": opLoadHook, "load_memory_entry_point": opLoadMemoryEntryPoint,
		"warm_import": opWarmImport, "describe_memory_cli": opDescribeMemoryCLI, "shutdown": opShutdown,
		"build_cli": opBuildCLI, "run_cli": opRunCLI,
	}
}

type pyContextKey struct{}

// Go context carrying the contextvars copy of the plugin thread that sent a request
func withPyContext(ctx context.Context, pyctx Ref) context.Context {
	return context.WithValue(ctx, pyContextKey{}, pyctx)
}

func pyContextOf(ctx context.Context) Ref {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(pyContextKey{}).(Ref)
	return v
}

// Error carrying the remote exception fields the parent re-raises
type wireError struct{ fields map[string]any }

func (e *wireError) Error() string             { return str(e.fields["message"]) }
func (e *wireError) WireError() map[string]any { return e.fields }
func (e *wireError) PyExceptionMRO() []string {
	out := []string{str(e.fields["type"])}
	for _, b := range e.fields["bases"].([]any) {
		out = append(out, str(b))
	}
	return out
}

// Wire error of a failure, the class name, message and base names of the Python exception
func (rt *runtime) wireError(p py, err error) error {
	var pe *pyhost.PyError
	if !errors.As(err, &pe) || pe.Exception() == nil {
		return &wireError{map[string]any{"type": "RuntimeError", "message": err.Error(), "bases": []any{"Exception", "BaseException"}}}
	}
	exc := pe.Exception()
	typ := p.typeOf(exc)
	object := p.attr("builtins", "object")
	bases := []any{}
	for i, base := range p.items(p.get(typ, "__mro__")) {
		if i == 0 || p.is(base, object) {
			continue
		}
		bases = append(bases, p.str(p.get(base, "__name__")))
	}
	fields := map[string]any{"type": p.str(p.get(typ, "__name__")), "message": p.str(exc), "bases": bases}
	if p.isinstance(exc, p.attr("builtins", "SystemExit")) {
		fields["code"] = p.repr(p.get(exc, "code"))
	}
	return &wireError{fields}
}

// Serves one parent request in the origin's contextvars context with the serving variable set to its id
func (rt *runtime) handle(ctx context.Context, method string, params map[string]any, _ any) (any, error) {
	op := opTable[method]
	var result any
	err := rt.h.Do(func(g *pyhost.GIL) error {
		p := py{g: g}
		if op == nil {
			return run(g, func(p py) {
				throw(&pyhost.Exception{Type: "ValueError", Message: fmt.Sprintf("unknown plugin host request %s", p.repr(p.obj(method)))})
			})
		}
		err := run(g, func(p py) {
			pyctx := pyContextOf(ctx)
			if pyctx == nil {
				pyctx = p.call(p.attr("contextvars", "copy_context"))
			}
			body := p.fn("serve", func(p py, _ []any, _ map[string]any) any {
				token := p.m(rt.serving, "set", plugins.ServingRequest(ctx))
				defer func() { _ = p.attempt(func() { p.m(rt.serving, "reset", token) }, "BaseException") }()
				result = op(rt, p, ctx, params)
				return nil
			})
			p.m(pyctx, "run", body)
		})
		if err != nil {
			return rt.wireError(p, err)
		}
		return nil
	})
	if ferr := rt.flushParentReleases(ctx); ferr != nil && err == nil {
		return nil, ferr
	}
	if err != nil {
		var we *wireError
		if errors.As(err, &we) {
			return nil, we
		}
		return nil, err
	}
	return result, nil
}

// Awaits a plugin awaitable on the host loop inside the serving context, a plain value passing through
func (rt *runtime) await(p py, ctx context.Context, result Ref) Ref {
	if !p.truthy(p.m(p.imp("inspect"), "isawaitable", result)) {
		return result
	}
	taskCtx := p.call(p.attr("contextvars", "copy_context"))
	var out Ref
	var err error
	rt.blocking(p.g, func() { out, err = result.AwaitIn(ctx, taskCtx) })
	if err != nil {
		throw(err)
	}
	return out
}

// Encodes an answer, keeping live client parts, provider modules and provider objects in the host by reference
func (rt *runtime) encodeResult(p py, owner string, value Ref) any {
	return rt.encode(p, value, func(p py, o Ref) map[string]any {
		if strings.HasPrefix(owner, profileClientOwner) && isLiveClientPart(p, o) {
			return rt.describeObject(p, owner, o)
		}
		if strings.HasPrefix(owner, providerModuleOwner) {
			if p.isType(o) || p.isinstance(o, p.attr("types", "ModuleType")) {
				return rt.describeNamespace(p, owner, o)
			}
			if isProviderObject(p, o) && isLiveClientPart(p, o) {
				return rt.describeObject(p, owner, o)
			}
		}
		if providerBase(p, o) != nil {
			return rt.describeObject(p, owner, o)
		}
		return rt.refFor(p, owner, o, false)
	})
}

// Positional and keyword arguments the parent sent, its callables becoming ParentCallable objects
func (rt *runtime) decodeArgs(p py, params map[string]any, owner string) ([]any, map[string]any) {
	var resolve resolveFunc
	resolve = func(p py, ref map[string]any) Ref {
		if _, ok := ref["__parent_event__"]; ok {
			return rt.resolveEvent(p, ref)
		}
		if _, ok := ref["__child_event__"]; ok {
			return rt.resolveEvent(p, ref)
		}
		if id, ok := ref["__parent_callable__"]; ok {
			return rt.newParentCallable(p, toInt(id), owner)
		}
		if id, ok := ref["__parent_object__"]; ok {
			return rt.newParentObject(p, toInt(id), str(ref["type"]))
		}
		out := map[string]any{}
		for k, v := range ref {
			out[k] = rt.decodeValue(p, v, resolve)
		}
		return p.obj(out)
	}
	args := []any{}
	for _, a := range p.items(rt.decode(p, orList(params["args"]), resolve)) {
		args = append(args, a)
	}
	kwargs := map[string]any{}
	kw := rt.decode(p, orEmptyMap(params["kwargs"]), resolve)
	for _, kv := range p.items(p.m(kw, "items")) {
		kwargs[p.str(p.item(kv, 0))] = p.item(kv, 1)
	}
	return args, kwargs
}

func orList(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

func opHello(_ *runtime, p py, _ context.Context, _ map[string]any) any {
	version := strings.Fields(p.str(p.attr("sys", "version")))[0]
	return map[string]any{"protocol": plugins.ProtocolVersion, "pid": int64(os.Getpid()), "python": version}
}

func opLoad(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	key := str(params["plugin_key"])
	rt.mu.Lock()
	target := rt.predeclared[key]
	delete(rt.predeclared, key)
	rt.mu.Unlock()
	if target == nil || !truthyParam(params["predeclared"]) || !p.is(p.m(p.attr("sys", "modules"), "get", p.str(p.get(target, "__name__"))), target) {
		target = importPlugin(p, params)
	}
	isModule := p.isinstance(target, p.attr("types", "ModuleType"))
	register := target
	if !(p.callable(target) && !isModule) {
		register = p.getOr(target, "register")
	}
	if register == nil || !p.callable(register) {
		throw(&pyhost.Exception{Type: "AttributeError", Message: fmt.Sprintf("Plugin '%s' has no register() function", pyStr(params["name"]))})
	}
	var moduleName any
	if n := p.getOr(target, "__name__"); n != nil && p.truthy(n) {
		moduleName = p.val(n)
	} else if n := p.getOr(register, "__module__"); n != nil {
		moduleName = p.val(n)
	}
	rt.mu.Lock()
	rt.modules[key] = optionalString(moduleName)
	rt.mu.Unlock()
	rt.await(p, ctx, p.call(register, rt.ctxObject(p, newPluginCtx(p, rt, key, params))))
	module := target
	if !isModule && p.callable(target) {
		modName := ""
		if m := p.getOr(target, "__module__"); m != nil {
			modName = p.str(m)
		}
		module = p.m(p.attr("sys", "modules"), "get", modName)
	}
	return map[string]any{"module": moduleName, "file": optionalAttr(p, module, "__file__")}
}

// Python object for a Go context
func (rt *runtime) ctxObject(p py, c *pluginCtx) Ref { return p.own(p.g.NewObject(c)) }

func optionalString(v any) string {
	if v == nil {
		return ""
	}
	return pyStr(v)
}

func truthyParam(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int64:
		return x != 0
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

func opLoadTools(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	key := str(params["plugin_key"])
	pkg := importPlugin(p, params)
	name := p.str(p.get(pkg, "__name__"))
	rt.mu.Lock()
	rt.modules[key] = name
	rt.predeclared[key] = p.keep(pkg)
	rt.mu.Unlock()
	tools := p.m(p.imp("importlib"), "import_module", name+".tools")
	registerTools := p.getOr(tools, "register_tools")
	if registerTools == nil || p.isNone(registerTools) {
		return map[string]any{"module": name, "register_tools": false, "file": optionalAttr(p, pkg, "__file__")}
	}
	rt.await(p, ctx, p.call(registerTools, rt.ctxObject(p, newPluginCtx(p, rt, key, params))))
	return map[string]any{"module": name, "register_tools": true, "file": optionalAttr(p, pkg, "__file__")}
}

func opLoadInstance(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	key := str(params["plugin_key"])
	if truthyParam(params["in_process"]) {
		return rt.loadBundledInstance(p, key, params)
	}
	path := pyStr(params["path"])
	rt.mu.Lock()
	module := rt.instanceModules[path]
	rt.mu.Unlock()
	if module == nil {
		module = importPlugin(p, params)
		rt.mu.Lock()
		rt.instanceModules[path] = p.keep(module)
		rt.mu.Unlock()
	}
	rt.mu.Lock()
	rt.modules[key] = p.str(p.get(module, "__name__"))
	rt.mu.Unlock()
	base := importBase(p, pyStr(params["base"]))
	c := newPluginCtx(p, rt, key, params)
	c.mode, c.capture, c.base, c.forward = ctxCapturing, pyStr(params["capture"]), p.keep(base), truthyParam(params["forward"])
	if register := p.getOr(module, "register"); register != nil && p.callable(register) {
		if err := p.attempt(func() { rt.await(p, ctx, p.call(register, rt.ctxObject(p, c))) }, "Exception"); err != nil && c.collectedValue() == nil {
			p.m(rt.logger, "debug", "register() failed for %s: %s", params["name"], err.(*pyhost.PyError).Exception())
		}
	}
	captured := c.collectedValue()
	if captured == nil {
		captured = instantiateSubclass(p, module, base)
	}
	if captured == nil {
		return nil
	}
	return rt.describeObject(p, key, captured)
}

// First base subclass among a namespace's attributes that constructs, nil when none does
func instantiateSubclass(p py, namespace, base Ref) Ref {
	for _, name := range p.dir(namespace) {
		attr := p.getOr(namespace, name)
		if attr == nil || !p.isType(attr) || !p.issubclass(attr, base) || p.is(attr, base) {
			continue
		}
		var made Ref
		if p.attempt(func() { made = p.call(attr) }, "Exception") == nil {
			return made
		}
	}
	return nil
}

func (rt *runtime) loadBundledInstance(p py, key string, params map[string]any) any {
	loader := p.imp("plugins.plugin_loader")
	module := p.mKw(loader, "load_plugin_module", []any{pyStr(params["module_name"]), pathOf(p, params["path"])}, map[string]any{
		"parents": p.call(p.attr("builtins", "tuple"), orList(params["parents"])), "logger": rt.logger,
		"plugins_root": pathOf(p, params["plugins_root"])})
	if p.isNone(module) {
		return nil
	}
	rt.mu.Lock()
	rt.modules[key] = p.str(p.get(module, "__name__"))
	rt.mu.Unlock()
	base := importBase(p, pyStr(params["base"]))
	c := newPluginCtx(p, rt, key, params)
	c.mode, c.capture, c.class = ctxCollector, pyStr(params["capture"]), str(params["collector_class"])
	instance := p.mKw(loader, "instance_from_module", []any{module}, map[string]any{"collector": rt.ctxObject(p, c),
		"collected_attr": "collected", "base_cls": base, "name": pyStr(params["name"]), "logger": rt.logger})
	if p.isNone(instance) {
		return nil
	}
	return rt.describeObject(p, key, instance)
}

func pathOf(p py, v any) Ref { return p.call(p.attr("pathlib", "Path"), pyStr(v)) }

func opLoadComputerUseProvider(rt *runtime, p py, _ context.Context, params map[string]any) any {
	loader := p.imp("plugins.plugin_loader")
	providerBase := p.attr("tools.computer_use.backend", "ComputerUseProvider")
	key := str(params["plugin_key"])
	var synthetic any
	if truthyParam(params["synthetic_namespace"]) {
		synthetic = params["synthetic_namespace"]
	}
	module := p.mKw(loader, "load_plugin_module", []any{pyStr(params["module_name"]), pathOf(p, params["path"])}, map[string]any{
		"parents": []any{"plugins", "plugins.computer_use"}, "logger": rt.logger, "synthetic_namespace": synthetic})
	if p.isNone(module) {
		return nil
	}
	rt.mu.Lock()
	rt.modules[key] = p.str(p.get(module, "__name__"))
	rt.mu.Unlock()
	collector := rt.computerUseCollector(p)
	instance := p.mKw(loader, "instance_from_module", []any{module}, map[string]any{"collector": collector,
		"collected_attr": "provider", "base_cls": providerBase, "name": pyStr(params["name"]), "logger": rt.logger})
	if p.isNone(instance) {
		return nil
	}
	return rt.describeObject(p, key, instance)
}

func opLoadMemoryEntryPoint(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	key, name := str(params["plugin_key"]), str(params["name"])
	ep := findEntryPoint(p, memoryEntryPointsGroup, name)
	if ep == nil {
		throw(&pyhost.Exception{Type: "ImportError", Message: fmt.Sprintf("Entry point '%s' not found in group '%s'", name, memoryEntryPointsGroup)})
	}
	loaded := p.m(ep, "load")
	rt.mu.Lock()
	rt.modules[key] = p.str(p.get(ep, "module"))
	rt.mu.Unlock()
	provider := rt.entryPointProvider(p, key, params, loaded, importBase(p, pyStr(params["base"])), name)
	if provider == nil {
		return nil
	}
	return rt.describeObject(p, key, provider)
}

func findEntryPoint(p py, group, name string) Ref {
	eps := p.mKw(p.m(p.imp("importlib.metadata"), "entry_points"), "select", nil, map[string]any{"group": group})
	for _, ep := range p.items(eps) {
		if p.str(p.get(ep, "name")) == name {
			return ep
		}
	}
	return nil
}

func (rt *runtime) entryPointProvider(p py, key string, params map[string]any, loaded, base Ref, name string) Ref {
	if p.isinstance(loaded, base) {
		return loaded
	}
	if p.isType(loaded) && p.issubclass(loaded, base) {
		var made Ref
		if p.attempt(func() { made = p.call(loaded) }, "Exception") == nil {
			return made
		}
	}
	if p.has(loaded, "register") {
		c := newPluginCtx(p, rt, key, params)
		c.mode = ctxProviderCollector
		obj := rt.ctxObject(p, c)
		p.mKw(obj, "collect", []any{p.get(loaded, "register")}, map[string]any{"source": optionalAttr(p, loaded, "__file__")})
		if provider := c.collectedValue(); provider != nil && p.truthy(provider) {
			return provider
		}
	}
	if p.callable(loaded) {
		var provider Ref
		if p.attempt(func() { provider = p.call(loaded) }, "TypeError") == nil && p.isinstance(provider, base) {
			return provider
		}
		c := newPluginCtx(p, rt, key, params)
		c.mode = ctxProviderCollector
		p.m(rt.ctxObject(p, c), "collect", loaded)
		return c.collectedValue()
	}
	provider := instantiateSubclass(p, loaded, base)
	if provider == nil {
		p.m(rt.logger, "debug", "Memory provider entry point '%s' loaded no provider", name)
	}
	return provider
}

func opWarmImport(rt *runtime, p py, _ context.Context, params map[string]any) any {
	name := str(params["name"])
	imported := false
	err := p.attempt(func() {
		if truthyParam(params["path"]) {
			imported = !p.isNone(rt.loadProviderPackage(p, params))
		} else if ep := findEntryPoint(p, memoryEntryPointsGroup, name); ep != nil {
			p.m(ep, "load")
			imported = true
		}
	}, "Exception")
	if err != nil {
		p.mKw(rt.logger, "debug", []any{"memory provider '%s' warm-up import failed", name}, map[string]any{"exc_info": true})
	}
	if imported {
		for _, module := range orList(params["native_warm"]).([]any) {
			if p.attempt(func() { p.m(p.imp("importlib"), "import_module", pyStr(module)) }, "Exception") != nil {
				p.mKw(rt.logger, "debug", []any{"warm-up import of %s skipped", module}, map[string]any{"exc_info": true})
			}
		}
	}
	return imported
}

// Provider package the memory loader names, through plugins.plugin_loader.load_plugin_module
func (rt *runtime) loadProviderPackage(p py, params map[string]any) Ref {
	parents := []any{}
	for _, parent := range orList(params["parents"]).([]any) {
		parents = append(parents, pyStr(parent))
	}
	return p.mKw(p.imp("plugins.plugin_loader"), "load_plugin_module", []any{pyStr(params["module_name"]), pathOf(p, params["path"])},
		map[string]any{"parents": p.call(p.attr("builtins", "tuple"), parents), "logger": rt.logger,
			"synthetic_namespace": params["synthetic_namespace"], "plugins_root": pathOf(p, params["plugins_root"])})
}

// Provider package or one submodule of it, nil when the package fails to import
func (rt *runtime) providerSubmodule(p py, params map[string]any) Ref {
	pkg := rt.loadProviderPackage(p, params)
	if p.isNone(pkg) {
		return nil
	}
	if sub := pyStr(params["submodule"]); params["submodule"] != nil && sub != "" {
		return p.m(p.imp("importlib"), "import_module", p.str(p.get(pkg, "__name__"))+"."+sub)
	}
	return pkg
}

func opDescribeMemoryCLI(_ *runtime, p py, _ context.Context, params map[string]any) any {
	cli := importMemoryCLI(p, pyStr(params["module_name"]), pyStr(params["path"]))
	register := p.getOr(cli, "register_cli")
	handler := p.getOr(cli, pyStr(params["name"])+"_command")
	return map[string]any{"register_cli": register != nil && p.callable(register), "handler": handler != nil && !p.isNone(handler)}
}

// The provider's cli.py imported under package shells without running the provider package
func importMemoryCLI(p py, moduleName, pluginDir string) Ref {
	cliName := moduleName + ".cli"
	modules := p.attr("sys", "modules")
	if mod := p.m(modules, "get", cliName); !p.isNone(mod) {
		return mod
	}
	register := p.attr("plugins.plugin_loader", "register_synthetic_package")
	p.call(register, userMemoryNamespace, []any{})
	p.call(register, moduleName, []any{pluginDir})
	util := p.imp("importlib.util")
	spec := p.m(util, "spec_from_file_location", cliName, filepath.Join(pluginDir, "cli.py"))
	if p.isNone(spec) || p.isNone(p.get(spec, "loader")) {
		return p.obj(nil)
	}
	mod := p.m(util, "module_from_spec", spec)
	p.m(modules, "__setitem__", cliName, mod)
	p.m(p.get(spec, "loader"), "exec_module", mod)
	return mod
}

func opInvoke(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	ref := toInt(params["ref"])
	fn, owner := rt.lookup(ref)
	if fn == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host callable %d was released", ref)})
	}
	args, kwargs := rt.decodeArgs(p, params, owner)
	for i, ref := range orList(params["restore"]).([]any) {
		if cls, ok := ref.(string); ok && cls != "" && i < len(args) {
			args[i] = restoreFrozen(p, importBase(p, cls), p.obj(args[i]))
		}
	}
	result := rt.await(p, ctx, p.callKw(fn, args, kwargs))
	if judge, ok := params["judge"].(string); ok && judge != "" {
		return map[string]any{"value": rt.encodeResult(p, owner, result), "is_instance": isBaseInstance(p, result, judge)}
	}
	return rt.encodeResult(p, owner, result)
}

// Instance of a frozen dataclass built from the dict the parent sent, its lists becoming the tuples the fields hold
func restoreFrozen(p py, cls, fields Ref) Ref {
	kw := map[string]any{}
	for _, kv := range p.items(p.m(fields, "items")) {
		v := p.item(kv, 1)
		if p.isinstance(v, p.attr("builtins", "list")) {
			v = p.call(p.attr("builtins", "tuple"), v)
		}
		kw[p.str(p.item(kv, 0))] = v
	}
	return p.callKw(cls, nil, kw)
}

func opObjInvoke(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	ref := toInt(params["ref"])
	obj, owner := rt.lookup(ref)
	if obj == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %d was released", ref)})
	}
	method := pyStr(params["method"])
	names := strings.Split(method, ".")
	for _, n := range names {
		if strings.HasPrefix(n, "_") {
			throw(&pyhost.Exception{Type: "AttributeError", Message: method})
		}
	}
	target := obj
	for _, n := range names[:len(names)-1] {
		target = p.get(target, n)
	}
	args, kwargs := rt.decodeArgs(p, params, owner)
	if base := providerBase(p, target); base != nil {
		argRefs := make([]Ref, len(args))
		for i, a := range args {
			argRefs[i] = a.(Ref)
		}
		kwRefs := map[string]Ref{}
		for k, v := range kwargs {
			kwRefs[k] = v.(Ref)
		}
		restored, restoredKw := rt.restoreArguments(p, declaredMember(p, base, names[len(names)-1]), argRefs, kwRefs)
		args = make([]any, len(restored))
		for i, a := range restored {
			args[i] = a
		}
		kwargs = map[string]any{}
		for k, v := range restoredKw {
			kwargs[k] = v
		}
	}
	return rt.encodeResult(p, owner, rt.await(p, ctx, p.callKw(p.get(target, names[len(names)-1]), args, kwargs)))
}

func opObjGetattr(rt *runtime, p py, _ context.Context, params map[string]any) any {
	ref := toInt(params["ref"])
	obj, owner := rt.lookup(ref)
	name := pyStr(params["name"])
	if obj == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %s was released", pyStr(params["ref"]))})
	}
	if strings.HasPrefix(name, "_") || !p.has(obj, name) {
		return map[string]any{"__missing__": name}
	}
	return rt.encodeResult(p, owner, p.get(obj, name))
}

func opObjOverrides(rt *runtime, p py, _ context.Context, params map[string]any) any {
	obj, _ := rt.lookup(toInt(params["ref"]))
	if obj == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %s was released", pyStr(params["ref"]))})
	}
	name := pyStr(params["method"])
	mine := p.getOr(p.typeOf(obj), name)
	base := p.getOr(importBase(p, pyStr(params["base"])), name)
	if mine == nil || base == nil {
		return mine != nil || base != nil
	}
	return !p.is(mine, base)
}

func opObjNext(rt *runtime, p py, _ context.Context, params map[string]any) any {
	ref := toInt(params["ref"])
	rt.mu.Lock()
	obj := rt.refs[ref]
	iterator := rt.iterators[ref]
	owner := rt.owners[ref]
	rt.mu.Unlock()
	if obj == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host object %d was released", ref)})
	}
	if iterator == nil {
		iterator = p.keep(p.call(p.attr("builtins", "iter"), obj))
		rt.mu.Lock()
		rt.iterators[ref] = iterator
		rt.mu.Unlock()
	}
	var item Ref
	if p.attempt(func() { item = p.call(p.attr("builtins", "next"), iterator) }, "StopIteration") != nil {
		return map[string]any{"done": true}
	}
	return map[string]any{"done": false, "item": rt.encodeResult(p, owner, item)}
}

func opEvent(rt *runtime, p py, _ context.Context, params map[string]any) any {
	event := rt.resolveEvent(p, map[string]any{"__child_event__": params["ref"]})
	switch op := params["op"]; op {
	case "is_set":
		return p.truthy(p.m(event, "is_set"))
	case "set":
		p.m(event, "set")
		return nil
	case "clear":
		p.m(event, "clear")
		return nil
	case "wait":
		return p.truthy(p.m(event, "wait", params["timeout"]))
	case "reason":
		reason := p.getOr(event, "reason")
		if reason == nil || !p.truthy(reason) {
			return ""
		}
		return p.str(reason)
	default:
		throw(&pyhost.Exception{Type: "ValueError", Message: fmt.Sprintf("unknown plugin host event operation %s", p.repr(p.obj(op)))})
	}
	return nil
}

func opRelease(rt *runtime, _ py, _ context.Context, params map[string]any) any {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, ref := range orList(params["refs"]).([]any) {
		id := toInt(ref)
		delete(rt.refs, id)
		delete(rt.owners, id)
		delete(rt.iterators, id)
	}
	return nil
}

func opConfigSchema(rt *runtime, p py, _ context.Context, params map[string]any) any {
	path := pyStr(params["path"])
	util := p.imp("importlib.util")
	spec := p.m(util, "spec_from_file_location", "_icebath_memory_config_schema."+filepath.Base(filepath.Dir(path)), path)
	if p.isNone(spec) || p.isNone(p.get(spec, "loader")) {
		throw(&pyhost.Exception{Type: "ImportError", Message: "cannot load " + path})
	}
	module := p.m(util, "module_from_spec", spec)
	p.m(p.get(spec, "loader"), "exec_module", module)
	schema := p.getOr(module, "CONFIG_SCHEMA")
	if schema == nil {
		return nil
	}
	return rt.encode(p, schema, nil)
}

func opLoadHook(rt *runtime, p py, _ context.Context, params map[string]any) any {
	moduleName := "icebath_hook_" + pyStr(params["hook_name"])
	util := p.imp("importlib.util")
	spec := p.m(util, "spec_from_file_location", moduleName, pathOf(p, params["path"]))
	if p.isNone(spec) || p.isNone(p.get(spec, "loader")) {
		return map[string]any{"skip": "could not load handler.py"}
	}
	module := p.m(util, "module_from_spec", spec)
	modules := p.attr("sys", "modules")
	p.m(modules, "__setitem__", moduleName, module)
	if err := p.attempt(func() { p.m(p.get(spec, "loader"), "exec_module", module) }, "BaseException"); err != nil {
		p.m(modules, "pop", moduleName, nil)
		throw(err)
	}
	handle := p.getOr(module, "handle")
	if handle == nil || p.isNone(handle) {
		return map[string]any{"skip": "no 'handle' function found"}
	}
	return rt.encode(p, p.obj(map[string]any{"handle": handle}), func(p py, o Ref) map[string]any { return rt.refFor(p, moduleName, o, false) })
}

func opObjSetattr(rt *runtime, p py, _ context.Context, params map[string]any) any {
	obj, _ := rt.lookup(toInt(params["ref"]))
	name := pyStr(params["name"])
	if obj == nil || strings.HasPrefix(name, "_") {
		throw(&pyhost.Exception{Type: "AttributeError", Message: name})
	}
	p.set(obj, name, rt.decode(p, params["value"], nil))
	return nil
}

func opUnload(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	return rt.unloadPlugin(p, ctx, str(params["plugin_key"]))
}

func (rt *runtime) unloadPlugin(p py, ctx context.Context, key string) any {
	rt.mu.Lock()
	callbacks := rt.unloadCallbacks[key]
	delete(rt.unloadCallbacks, key)
	for i, k := range rt.unloadOrder {
		if k == key {
			rt.unloadOrder = append(rt.unloadOrder[:i], rt.unloadOrder[i+1:]...)
			break
		}
	}
	rt.mu.Unlock()
	errs := []any{}
	for i := len(callbacks) - 1; i >= 0; i-- {
		cb := callbacks[i]
		if err := p.attempt(func() { rt.await(p, ctx, p.call(cb)) }, "Exception"); err != nil {
			var pe *pyhost.PyError
			if errors.As(err, &pe) {
				errs = append(errs, pe.Name()+": "+pe.Message)
			}
		}
	}
	rt.mu.Lock()
	for ref, owner := range rt.owners {
		if owner == key {
			delete(rt.refs, ref)
			delete(rt.owners, ref)
		}
	}
	moduleName := rt.modules[key]
	delete(rt.modules, key)
	rt.mu.Unlock()
	if moduleName != "" {
		evictModules(p, moduleName)
	}
	return map[string]any{"errors": errs}
}

func opEvictModules(_ *runtime, p py, _ context.Context, params map[string]any) any {
	evictModules(p, pyStr(params["module_name"]))
	return nil
}

func opShutdown(rt *runtime, p py, ctx context.Context, _ map[string]any) any {
	rt.mu.Lock()
	keys := append([]string{}, rt.unloadOrder...)
	rt.mu.Unlock()
	for _, key := range keys {
		rt.unloadPlugin(p, ctx, key)
	}
	rt.stop()
	return nil
}

// Drops a module and every submodule from sys.modules, iterating a snapshot of the keys
func evictModules(p py, moduleName string) {
	modules := p.attr("sys", "modules")
	eq := p.attr("operator", "eq")
	prefix := moduleName + "."
	for _, name := range p.items(p.call(p.attr("builtins", "list"), modules)) {
		if p.truthy(p.call(eq, name, moduleName)) || p.truthy(p.m(name, "startswith", prefix)) {
			p.m(modules, "pop", name, nil)
		}
	}
}

// Imports a directory plugin under the module name the parent assigned, or resolves an entry point
func importPlugin(p py, params map[string]any) Ref {
	if truthyParam(params["entrypoint"]) {
		eps := p.mKw(p.m(p.imp("importlib.metadata"), "entry_points"), "select", nil, map[string]any{"group": entryPointsGroup})
		for _, ep := range p.items(eps) {
			if p.val(p.get(ep, "name")) == params["name"] {
				return p.m(ep, "load")
			}
		}
		throw(&pyhost.Exception{Type: "ImportError", Message: fmt.Sprintf("Entry point '%s' not found in group '%s'", pyStr(params["name"]), entryPointsGroup)})
	}
	pluginDir := pyStr(params["path"])
	initFile := filepath.Join(pluginDir, "__init__.py")
	if _, err := os.Stat(initFile); err != nil {
		throw(&pyhost.Exception{Type: "FileNotFoundError", Message: "No __init__.py in " + pluginDir})
	}
	moduleName := pyStr(params["module_name"])
	modules := p.attr("sys", "modules")
	parts := strings.Split(moduleName, ".")
	parts = parts[:len(parts)-1]
	for i := 1; i <= len(parts); i++ {
		parent := strings.Join(parts[:i], ".")
		if p.truthy(p.m(modules, "__contains__", parent)) {
			continue
		}
		ns := p.call(p.attr("types", "ModuleType"), parent)
		p.set(ns, "__path__", []any{})
		p.set(ns, "__package__", parent)
		p.m(modules, "__setitem__", parent, ns)
	}
	evictModules(p, moduleName)
	util := p.imp("importlib.util")
	spec := p.mKw(util, "spec_from_file_location", []any{moduleName, pathOf(p, initFile)}, map[string]any{"submodule_search_locations": []any{pluginDir}})
	if p.isNone(spec) || p.isNone(p.get(spec, "loader")) {
		throw(&pyhost.Exception{Type: "ImportError", Message: "Cannot create module spec for " + initFile})
	}
	module := p.m(util, "module_from_spec", spec)
	p.set(module, "__package__", moduleName)
	p.set(module, "__path__", []any{pluginDir})
	p.m(modules, "__setitem__", moduleName, module)
	if err := p.attempt(func() { p.m(p.get(spec, "loader"), "exec_module", module) }, "BaseException"); err != nil {
		evictModules(p, moduleName)
		throw(err)
	}
	return module
}

// Class a module and attribute reference names
func importBase(p py, ref string) Ref {
	module, attr, _ := strings.Cut(ref, ":")
	return p.get(p.m(p.imp("importlib"), "import_module", module), attr)
}

// Provider class among the already imported host object bases that value implements, nil when none
func providerBase(p py, value Ref) Ref {
	refs := make([]string, 0, len(plugins.HostObjectBases)+len(plugins.HostResultObjectBases))
	for _, method := range sortedKeys(plugins.HostObjectBases) {
		refs = append(refs, plugins.HostObjectBases[method])
	}
	refs = append(refs, plugins.HostResultObjectBases...)
	modules := p.attr("sys", "modules")
	for _, ref := range refs {
		module, attr, _ := strings.Cut(ref, ":")
		mod := p.m(modules, "get", module)
		if p.isNone(mod) {
			continue
		}
		base := p.getOr(mod, attr)
		if base != nil && p.isType(base) && p.isinstance(value, base) {
			return base
		}
	}
	return nil
}

// Whether a value reached from a live SDK client stays in the host as an object instead of data
func isLiveClientPart(p py, value Ref) bool {
	if p.callable(value) || p.isType(value) || p.truthy(p.call(p.attr("dataclasses", "is_dataclass"), value)) {
		return false
	}
	dump := p.getOr(value, "model_dump")
	return dump == nil || !p.callable(dump)
}
