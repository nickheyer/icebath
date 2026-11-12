package pluginhost

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyhost"
)

// Profiles of a plugin directory or provider entry point, captured again after a host restart
func (rt *runtime) profilesFor(p py, ctx context.Context, params map[string]any) *profileSet {
	path := pyStr(params["path"])
	rt.mu.Lock()
	set := rt.profiles[path]
	rt.mu.Unlock()
	if set != nil {
		return set
	}
	var captured []Ref
	if strings.HasPrefix(path, entryPointProfileKey) {
		captured = rt.captureEntryPoint(p, ctx, strings.TrimPrefix(path, entryPointProfileKey), pyStr(params["module_name"]))
	} else {
		captured = rt.captureProfiles(p, path, pyStr(params["module_name"]))
	}
	set = &profileSet{byName: map[string]Ref{}}
	for _, profile := range captured {
		set.add(p, profile)
	}
	rt.mu.Lock()
	rt.profiles[path] = set
	rt.mu.Unlock()
	return set
}

func (s *profileSet) get(name string) Ref {
	profile := s.byName[name]
	if profile == nil {
		throw(&pyhost.Exception{Type: "KeyError", Message: name})
	}
	return profile
}

// Profiles a provider entry point registers on load plus on its zero argument call
func (rt *runtime) captureEntryPoint(p py, ctx context.Context, name, value string) []Ref {
	var captured []Ref
	rt.capturing(p, func() {
		loaded := p.m(rt.entryPoint(p, name, value), "load")
		if p.callable(loaded) && !requiresArguments(p, loaded) {
			rt.await(p, ctx, p.call(loaded))
		}
	}, &captured)
	return captured
}

func (rt *runtime) entryPoint(p py, name, value string) Ref {
	return p.callKw(p.attr("importlib.metadata", "EntryPoint"), nil, map[string]any{"name": name, "value": value, "group": entryPointsGroup})
}

// Imports a model provider plugin and returns the profiles it passes to register_provider
func (rt *runtime) captureProfiles(p py, path, moduleName string) []Ref {
	p.m(p.imp("providers"), "list_providers")
	var captured []Ref
	rt.capturing(p, func() {
		importPlugin(p, map[string]any{"path": path, "module_name": moduleName, "name": lastPathPart(path)})
	}, &captured)
	return captured
}

func lastPathPart(path string) string {
	path = strings.TrimRight(path, "/\\")
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// Collects every register_provider call made inside body, even through a name bound earlier
func (rt *runtime) capturing(p py, body func(), out *[]Ref) {
	providers := p.imp("providers")
	p.m(providers, "list_providers")
	rt.mu.Lock()
	if rt.captureLock == nil {
		rt.captureLock = p.keep(p.call(p.attr("threading", "RLock")))
		rt.capturingRegister = p.keep(p.fn("_capturing_register", func(p py, args []any, kw map[string]any) any {
			rt.mu.Lock()
			depth := len(rt.captures)
			rt.mu.Unlock()
			if depth > 0 {
				rt.mu.Lock()
				rt.captures[depth-1] = append(rt.captures[depth-1], p.keep(p.obj(args[0])))
				rt.mu.Unlock()
				return nil
			}
			return p.call(rt.originalRegister, args...)
		}))
	}
	lock, register := rt.captureLock, rt.capturingRegister
	rt.mu.Unlock()
	p.m(lock, "acquire")
	rt.mu.Lock()
	rt.captures = append(rt.captures, nil)
	rt.mu.Unlock()
	original := p.get(providers, "register_provider")
	if !p.is(original, register) {
		rt.originalRegister = p.keep(original)
		p.set(providers, "register_provider", register)
	}
	defer func() {
		rt.mu.Lock()
		*out = rt.captures[len(rt.captures)-1]
		rt.captures = rt.captures[:len(rt.captures)-1]
		rt.mu.Unlock()
		p.set(providers, "register_provider", original)
		p.m(lock, "release")
	}()
	body()
}

// Whether fn cannot be called with zero arguments, unintrospectable callables counting as zero argument
func requiresArguments(p py, fn Ref) bool {
	var sig Ref
	if p.attempt(func() { sig = p.m(p.imp("inspect"), "signature", fn) }, "TypeError", "ValueError") != nil {
		return false
	}
	empty := p.get(p.attr("inspect", "Parameter"), "empty")
	for _, prm := range p.items(p.m(p.get(sig, "parameters"), "values")) {
		kind := p.val(p.call(p.attr("builtins", "int"), p.get(prm, "kind"))).(int64)
		if (kind == 0 || kind == 1 || kind == 3) && p.is(p.get(prm, "default"), empty) {
			return true
		}
	}
	return false
}

// Why a profile cannot run behind the plugin host, empty when it can
func hostUnsupportedReason(p py, profile Ref) string {
	base := p.attr("providers.base", "ProviderProfile")
	if !p.is(p.get(p.typeOf(profile), "create_client"), p.get(base, "create_client")) {
		return "it overrides create_client(), which returns a live SDK client"
	}
	return ""
}

func (rt *runtime) extractionEntry(p py, profile Ref) map[string]any {
	entry := rt.describeProfile(p, profile)
	if reason := hostUnsupportedReason(p, profile); reason != "" {
		entry["in_process"] = reason
	}
	return entry
}

// Profile data plus the methods and callable fields that must run in the host
func (rt *runtime) describeProfile(p py, profile Ref) map[string]any {
	base := p.attr("providers.base", "ProviderProfile")
	fields := map[string]any{}
	calls := map[string]any{}
	for _, kv := range p.items(p.m(p.call(p.attr("builtins", "vars"), profile), "items")) {
		name := p.str(p.item(kv, 0))
		if strings.HasPrefix(name, "_") {
			continue
		}
		value := p.item(kv, 1)
		if p.callable(value) {
			calls[name] = map[string]any{"async": isAsyncCallable(p, value), "sig": describeSignature(p, value), "field": true}
		} else {
			fields[name] = rt.encode(p, value, nil)
		}
	}
	cls := p.typeOf(profile)
	sentinel := p.call(p.attr("builtins", "object"))
	for _, name := range p.dir(cls) {
		_, isField := fields[name]
		_, isCall := calls[name]
		if strings.HasPrefix(name, "_") || isField || isCall {
			continue
		}
		attr := p.get(cls, name)
		inherited := p.getOr(base, name)
		if inherited == nil {
			inherited = sentinel
		}
		if p.is(attr, inherited) {
			continue
		}
		static := p.call(p.attr("inspect", "getattr_static"), profile, name)
		if p.isinstance(static, p.attr("builtins", "property")) || !p.callable(attr) {
			fields[name] = rt.encode(p, p.get(profile, name), nil)
			continue
		}
		calls[name] = map[string]any{"async": isAsyncCallable(p, attr), "sig": describeSignature(p, p.get(profile, name)), "field": false}
	}
	return map[string]any{"name": p.val(p.get(profile, "name")), "type": p.typeName(profile), "fields": fields, "calls": calls}
}

func opProfileCall(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	target := p.get(rt.profilesFor(p, ctx, params).get(pyStr(params["profile"])), pyStr(params["attr"]))
	args, kwargs := rt.decodeArgs(p, params, "")
	return rt.encode(p, rt.await(p, ctx, p.callKw(target, args, kwargs)), nil)
}

func opProfileClient(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	profile := rt.profilesFor(p, ctx, params).get(pyStr(params["profile"]))
	args, kwargs := rt.decodeArgs(p, params, "")
	client := rt.await(p, ctx, p.callKw(p.get(profile, "create_client"), args, kwargs))
	return rt.describeObject(p, profileClientOwner+pyStr(params["path"])+":"+pyStr(params["profile"]), client)
}

func opProviderEntryPointLoad(rt *runtime, p py, _ context.Context, params map[string]any) any {
	name, value := pyStr(params["name"]), pyStr(params["value"])
	key := entryPointProfileKey + name
	reply := map[string]any{"callable": false, "requires_arguments": false}
	var captured []Ref
	rt.capturing(p, func() {
		var loaded Ref
		if err := p.attempt(func() { loaded = p.m(rt.entryPoint(p, name, value), "load") }, "Exception"); err != nil {
			reply["error"] = errorFields(err)
			return
		}
		rt.mu.Lock()
		rt.entryPoints[key] = p.keep(loaded)
		rt.mu.Unlock()
		reply["callable"] = p.callable(loaded)
		reply["requires_arguments"] = p.callable(loaded) && requiresArguments(p, loaded)
	}, &captured)
	reply["profiles"] = rt.recordProfiles(p, key, captured)
	return reply
}

func opProviderEntryPointInvoke(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	name, value := pyStr(params["name"]), pyStr(params["value"])
	key := entryPointProfileKey + name
	rt.mu.Lock()
	loaded := rt.entryPoints[key]
	rt.mu.Unlock()
	if loaded == nil {
		loaded = p.m(rt.entryPoint(p, name, value), "load")
		rt.mu.Lock()
		rt.entryPoints[key] = p.keep(loaded)
		rt.mu.Unlock()
	}
	reply := map[string]any{}
	var captured []Ref
	rt.capturing(p, func() {
		if err := p.attempt(func() { rt.await(p, ctx, p.call(loaded)) }, "Exception"); err != nil {
			reply["error"] = errorFields(err)
		}
	}, &captured)
	reply["profiles"] = rt.recordProfiles(p, key, captured)
	return reply
}

// Adds captured profiles to the key's set and returns their extraction entries
func (rt *runtime) recordProfiles(p py, key string, captured []Ref) []any {
	rt.mu.Lock()
	set := rt.profiles[key]
	if set == nil {
		set = &profileSet{byName: map[string]Ref{}}
		rt.profiles[key] = set
	}
	rt.mu.Unlock()
	entries := []any{}
	for _, profile := range captured {
		set.add(p, profile)
		entries = append(entries, rt.extractionEntry(p, profile))
	}
	return entries
}

// Type name and message of a Python failure
func errorFields(err error) map[string]any {
	if pe, ok := err.(*pyhost.PyError); ok {
		return map[string]any{"type": pe.Name(), "message": pe.Message}
	}
	return map[string]any{"type": "RuntimeError", "message": err.Error()}
}

func opProviderModuleCall(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	target := rt.providerSubmodule(p, params)
	if target == nil {
		return rt.encode(p, p.obj(map[string]any{"loaded": false}), nil)
	}
	function := p.get(target, pyStr(params["function"]))
	args, kwargs := rt.decodeArgs(p, params, "")
	result := rt.await(p, ctx, p.callKw(function, args, kwargs))
	return rt.encode(p, p.obj(map[string]any{"loaded": true, "result": result}), nil)
}

func opProviderModuleImport(rt *runtime, p py, _ context.Context, params map[string]any) any {
	pkg := rt.loadProviderPackage(p, params)
	if p.isNone(pkg) {
		return rt.encode(p, p.obj(map[string]any{"loaded": false}), nil)
	}
	target := pkg
	if sub := pyStr(params["submodule"]); params["submodule"] != nil && sub != "" {
		target = p.m(p.imp("importlib"), "import_module", p.str(p.get(pkg, "__name__"))+"."+sub)
	}
	described := rt.describeNamespace(p, providerModuleOwner+p.str(p.get(pkg, "__name__")), target)
	return rt.encode(p, p.obj(map[string]any{"loaded": true, "module": described}), nil)
}

// Positional arguments from JSON text, the listed positions rebuilt as paths
func jsonArguments(p py, params map[string]any) (Ref, map[int64]bool) {
	text := "[]"
	if s, ok := params["args_json"].(string); ok && s != "" {
		text = s
	}
	args := p.m(p.imp("json"), "loads", text)
	paths := map[int64]bool{}
	for _, index := range orList(params["path_args"]).([]any) {
		i := toInt(index)
		paths[i] = true
		p.m(args, "__setitem__", i, p.call(p.attr("pathlib", "Path"), p.item(args, i)))
	}
	return args, paths
}

func opProviderModuleJSONCall(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	target := rt.providerSubmodule(p, params)
	if target == nil {
		return map[string]any{"loaded": false}
	}
	for _, name := range orList(params["require"]).([]any) {
		p.get(target, pyStr(name))
	}
	function := p.get(target, pyStr(params["function"]))
	args, paths := jsonArguments(p, params)
	var tracked []any
	for _, path := range orList(params["track"]).([]any) {
		node := args
		for _, key := range path.([]any) {
			node = p.item(node, key)
		}
		tracked = append(tracked, node)
	}
	argItems := p.items(args)
	callArgs := make([]any, len(argItems))
	for i, a := range argItems {
		callArgs[i] = a
	}
	result := rt.await(p, ctx, p.call(function, callArgs...))
	isPath := p.isinstance(result, p.attr("pathlib", "PurePath"))
	slots := make([]any, len(argItems))
	for i, a := range argItems {
		if !paths[int64(i)] {
			slots[i] = a
		}
	}
	var r any = result
	if isPath {
		r = nil
	}
	if tracked == nil {
		tracked = []any{}
	}
	root := p.obj(map[string]any{"a": slots, "r": r, "t": tracked})
	resultKeys := []any{}
	for _, part := range tracked {
		keys := []any{}
		ref := part.(Ref)
		if p.isinstance(ref, p.attr("builtins", "dict")) {
			for _, kv := range p.items(p.m(ref, "items")) {
				if p.is(p.item(kv, 1), result) {
					keys = append(keys, p.item(kv, 0))
				}
			}
		}
		resultKeys = append(resultKeys, keys)
	}
	var resultPath any
	if isPath {
		resultPath = p.str(result)
	}
	var rootJSON any
	if p.attempt(func() {
		rootJSON = p.val(p.mKw(p.imp("json"), "dumps", []any{root}, map[string]any{"ensure_ascii": false}))
	}, "TypeError", "ValueError") != nil {
		rootJSON = nil
	}
	return map[string]any{"loaded": true, "root_json": rootJSON, "shared": sharedPaths(p, root),
		"result_keys": rt.encode(p, p.obj(resultKeys), nil), "result_is_path": isPath, "result_path": resultPath}
}

// Groups of key paths inside root that hold one and the same dict or list object
func sharedPaths(p py, root Ref) []any {
	paths := map[uintptr][]any{}
	var order []uintptr
	dict, list := p.attr("builtins", "dict"), p.attr("builtins", "list")
	var walk func(node Ref, path []any)
	walk = func(node Ref, path []any) {
		isDict := p.isinstance(node, dict)
		if (!isDict && !p.isinstance(node, list)) || len(path) > 64 {
			return
		}
		id := node.Ptr()
		if seen, ok := paths[id]; ok {
			paths[id] = append(seen, append([]any{}, path...))
			return
		}
		paths[id] = []any{append([]any{}, path...)}
		order = append(order, id)
		if isDict {
			for _, kv := range p.items(p.m(node, "items")) {
				walk(p.item(kv, 1), append(append([]any{}, path...), p.val(p.item(kv, 0))))
			}
			return
		}
		for i, item := range p.items(node) {
			walk(item, append(append([]any{}, path...), int64(i)))
		}
	}
	walk(root, nil)
	out := []any{}
	for _, id := range order {
		if len(paths[id]) > 1 {
			out = append(out, paths[id])
		}
	}
	return out
}

// Context managers of a provider submodule entered on one thread and left when the hold is released
type heldManagers struct {
	entered, released, done Ref
	mu                      sync.Mutex
	enterError, exitError   error
	injected                Ref
	raised                  bool
}

func (h *heldManagers) injectedError() Ref {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.injected
}

func opProviderModuleHold(rt *runtime, p py, _ context.Context, params map[string]any) any {
	target := rt.providerSubmodule(p, params)
	if target == nil {
		return map[string]any{"loaded": false}
	}
	threading := p.imp("threading")
	held := &heldManagers{entered: p.keep(p.m(threading, "Event")), released: p.keep(p.m(threading, "Event")), done: p.keep(p.m(threading, "Event"))}
	target = p.keep(target)
	specs := orList(params["managers"]).([]any)
	pyctx := p.keep(p.call(p.attr("contextvars", "copy_context")))
	hold := p.fn("_hold", func(p py, _ []any, _ map[string]any) any {
		err := p.attempt(func() {
			stack := p.call(p.attr("contextlib", "ExitStack"))
			p.m(stack, "__enter__")
			bodyErr := p.attempt(func() {
				for _, s := range specs {
					spec := s.(map[string]any)
					manager := p.get(target, pyStr(spec["name"]))
					if truthyParam(spec["call"]) {
						args, _ := jsonArguments(p, spec)
						items := p.items(args)
						callArgs := make([]any, len(items))
						for i, a := range items {
							callArgs[i] = a
						}
						manager = p.call(manager, callArgs...)
					}
					p.m(stack, "enter_context", manager)
				}
				p.m(held.entered, "set")
				p.m(held.released, "wait")
				if injected := held.injectedError(); injected != nil {
					throw(p.g.Raising(injected))
				}
			}, "BaseException")
			if bodyErr == nil {
				p.m(stack, "__exit__", nil, nil, nil)
				return
			}
			exc := bodyErr.(*pyhost.PyError).Exception()
			if !p.truthy(p.m(stack, "__exit__", p.typeOf(exc), exc, p.get(exc, "__traceback__"))) {
				throw(bodyErr)
			}
		}, "BaseException")
		if err != nil {
			exc := err.(*pyhost.PyError).Exception()
			injected := held.injectedError()
			held.mu.Lock()
			switch {
			case !p.truthy(p.m(held.entered, "is_set")):
				held.enterError = err
			case injected != nil && p.is(exc, injected):
				held.raised = true
			default:
				held.exitError = err
			}
			held.mu.Unlock()
		}
		p.m(held.entered, "set")
		p.m(held.done, "set")
		return nil
	})
	runHold := p.fn("run", func(p py, _ []any, _ map[string]any) any { return p.m(pyctx, "run", hold) })
	thread := p.callKw(p.get(threading, "Thread"), nil, map[string]any{"target": runHold, "name": "plugin-host-hold", "daemon": true})
	p.m(thread, "start")
	p.m(held.entered, "wait")
	held.mu.Lock()
	enterErr := held.enterError
	held.mu.Unlock()
	if enterErr != nil {
		throw(enterErr)
	}
	id := rt.remember(p, providerModuleOwner+"hold", p.obj(nil))
	rt.mu.Lock()
	rt.holds[id] = held
	rt.mu.Unlock()
	return map[string]any{"loaded": true, "hold": id}
}

func opProviderModuleRelease(rt *runtime, p py, _ context.Context, params map[string]any) any {
	id := toInt(params["hold"])
	rt.mu.Lock()
	held := rt.holds[id]
	delete(rt.holds, id)
	delete(rt.refs, id)
	delete(rt.owners, id)
	rt.mu.Unlock()
	if held == nil {
		throw(&pyhost.Exception{Type: "LookupError", Message: fmt.Sprintf("plugin host hold %s was released", pyStr(params["hold"]))})
	}
	if errSpec, ok := params["error"].(map[string]any); ok {
		builtins := p.imp("builtins")
		kind := p.getOr(builtins, pyStr(errSpec["type"]))
		if kind == nil || !p.isType(kind) || !p.issubclass(kind, p.get(builtins, "Exception")) {
			kind = p.get(builtins, "RuntimeError")
		}
		injected := p.keep(p.call(kind, pyStr(errSpec["message"])))
		held.mu.Lock()
		held.injected = injected
		held.mu.Unlock()
	}
	p.m(held.released, "set")
	p.m(held.done, "wait")
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.exitError != nil {
		throw(held.exitError)
	}
	return map[string]any{"suppressed": held.injected != nil && !held.raised}
}

func opASGI(rt *runtime, p py, ctx context.Context, params map[string]any) any {
	apiPath := p.str(p.m(p.call(p.attr("pathlib", "Path"), pyStr(params["dashboard_dir"])), "__truediv__", pyStr(params["api_file"])))
	rt.mu.Lock()
	app := rt.asgiApps[apiPath]
	rt.mu.Unlock()
	if app == nil {
		fastAPI := p.attr("fastapi", "FastAPI")
		moduleName := "icebath_dashboard_plugin_" + pyStr(params["plugin"])
		util := p.imp("importlib.util")
		spec := p.m(util, "spec_from_file_location", moduleName, pathOf(p, apiPath))
		if p.isNone(spec) || p.isNone(p.get(spec, "loader")) {
			throw(&pyhost.Exception{Type: "ImportError", Message: "cannot load dashboard api " + apiPath})
		}
		module := p.m(util, "module_from_spec", spec)
		p.m(p.attr("sys", "modules"), "__setitem__", moduleName, module)
		p.m(p.get(spec, "loader"), "exec_module", module)
		router := p.getOr(module, "router")
		if router == nil || p.isNone(router) {
			throw(&pyhost.Exception{Type: "AttributeError", Message: fmt.Sprintf("dashboard api %s has no 'router'", lastPathPart(apiPath))})
		}
		app = p.call(fastAPI)
		p.m(app, "include_router", router)
		rt.mu.Lock()
		rt.asgiApps[apiPath] = p.keep(app)
		rt.mu.Unlock()
	}
	httpx := p.imp("httpx")
	admitted := p.call(p.attr("icebath_cli.plugin_host_http_auth", "with_hosted_admission"), app, params["authenticated"])
	transport := p.callKw(p.get(httpx, "ASGITransport"), nil, map[string]any{"app": admitted})
	client := p.callKw(p.get(httpx, "AsyncClient"), nil, map[string]any{"transport": transport, "base_url": "http://plugin-host"})
	rt.await(p, ctx, p.m(client, "__aenter__"))
	var response Ref
	headers := []any{}
	for _, h := range orList(params["headers"]).([]any) {
		headers = append(headers, p.call(p.attr("builtins", "tuple"), h))
	}
	body := rt.decode(p, params["body"], nil)
	if !p.truthy(body) {
		body = p.obj([]byte{})
	}
	reqErr := p.attempt(func() {
		response = rt.await(p, ctx, p.mKw(client, "request", []any{pyStr(params["method"]), pyStr(params["path"])},
			map[string]any{"params": queryText(params["query"]), "headers": headers, "content": body}))
	}, "BaseException")
	if reqErr != nil {
		exc := reqErr.(*pyhost.PyError).Exception()
		if !p.truthy(rt.await(p, ctx, p.m(client, "__aexit__", p.typeOf(exc), exc, p.get(exc, "__traceback__")))) {
			throw(reqErr)
		}
		return nil
	}
	rt.await(p, ctx, p.m(client, "__aexit__", nil, nil, nil))
	result := map[string]any{"status": p.get(response, "status_code"), "headers": p.call(p.attr("builtins", "list"), p.m(p.get(response, "headers"), "multi_items")),
		"body": p.get(response, "content")}
	return rt.encode(p, p.obj(result), nil)
}

func queryText(v any) string {
	if v == nil {
		return ""
	}
	return pyStr(v)
}
