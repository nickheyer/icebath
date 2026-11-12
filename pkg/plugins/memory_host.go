package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	memoryProviderBase    = "agent.memory_provider:MemoryProvider"
	memoryProviderCapture = "register_memory_provider"
)

// Plugin context mode that routes a hosted memory provider's registrations the way the memory loader collector does
type memoryCollector struct {
	name           string
	registerSkills bool
	trackSkill     func(qualifiedName string)
	mu             sync.Mutex
	source         *hookSource
}

// Plugin context for a memory provider named name whose registrations go through a collector
func newMemoryCollectorContext(m *Manager, name string, registerSkills bool, trackSkill func(string)) *Context {
	c := newContext(&Manifest{Name: name, Key: name}, m)
	c.collector = &memoryCollector{name: name, registerSkills: registerSkills, trackSkill: trackSkill}
	return c
}

func (mc *memoryCollector) hookSource() *hookSource {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	return mc.source
}

func (mc *memoryCollector) setSource(src *hookSource) {
	mc.mu.Lock()
	mc.source = src
	mc.mu.Unlock()
}

// Disposes the fallback hooks the collector source holds under the discovery lock
func (mc *memoryCollector) dropHooks(ctx context.Context, m *Manager) {
	_, unlock := m.lockDiscovery(ctx)
	defer unlock()
	m.dropFallbackHooks(mc.hookSource())
}

// Answers one context call of the hosted provider as the collector attribute of that name would
func (mc *memoryCollector) serve(ctx context.Context, h *PluginHost, c *Context, method string, args []any, kwargs map[string]any) (any, error) {
	switch method {
	case "register_hook":
		return hostMethod(method, []pyParam{req("hook_name"), req("callback"), kwo("hold_stream", false)},
			func(ctx context.Context, _ *PluginHost, c *Context, a, _ map[string]any) (any, error) {
				cb, err := argCallable(method, "callback", a["callback"])
				if err != nil {
					return nil, err
				}
				hookName, hold := argStr(a["hook_name"]), config.PyTruthy(a["hold_stream"])
				_, unlock := c.manager.lockDiscovery(ctx)
				defer unlock()
				return c.manager.registerFallbackHookWith(c, mc.hookSource(), hookName, func() *Registration {
					return c.manager.trackHostedHook(c, hookName, cb, hold)
				}), nil
			})(ctx, h, c, args, kwargs)
	case "register_memory_provider", "register_cli_command":
		return nil, nil
	case "register_skill":
		if !mc.registerSkills {
			return nil, nil
		}
		if _, err := hostCtxCall(ctx, h, c, method, args, kwargs); err != nil {
			logDebug(fmt.Sprintf("Memory provider '%s' failed to register skill: %s", mc.name, err))
			return nil, nil
		}
		skill := kwargs["name"]
		if len(args) > 0 {
			skill = args[0]
		}
		qualified := mc.name + ":" + config.PyStr(skill)
		if _, ok := c.manager.FindPluginSkill(qualified); ok && mc.trackSkill != nil {
			mc.trackSkill(qualified)
		}
		return nil, nil
	}
	if !strings.HasPrefix(method, "register_") {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: method}
	}
	result, err := hostCtxCall(ctx, h, c, method, args, kwargs)
	if err != nil {
		logWarn(fmt.Sprintf("Memory provider '%s' failed to %s: %s", mc.name, method, err))
		return nil, nil
	}
	return result, nil
}

// Replays one plugin context method by Python name, an AttributeError for a name the context lacks
func hostCtxCall(ctx context.Context, h *PluginHost, c *Context, method string, args []any, kwargs map[string]any) (any, error) {
	fn, ok := hostCtxDispatch[method]
	if !ok {
		return nil, &HostBuiltinError{Type: "AttributeError", Message: fmt.Sprintf("'PluginContext' object has no attribute %s", config.PyRepr(method))}
	}
	return fn(ctx, h, c, args, kwargs)
}

// Sets the hook source a hosted entry point collector reports and drops that source's fallback hooks
func (h *PluginHost) serveCollectSource(ctx context.Context, params map[string]any) (any, error) {
	c, err := h.pluginCtx(params)
	if err != nil {
		return nil, err
	}
	if c.collector == nil {
		return nil, &HostBuiltinError{Type: "LookupError", Message: fmt.Sprintf("plugin %s does not collect a memory provider", config.PyRepr(params["plugin"]))}
	}
	var src *hookSource
	if file, ok := params["source"].(string); ok {
		src = hookSourceOf(c.collector.name, file)
	}
	c.collector.setSource(src)
	c.collector.dropHooks(ctx, c.manager)
	return nil, nil
}

// Loads a user memory provider package in the plugin host with its other registrations collected here
func CollectMemoryProviderInHost(ctx context.Context, name, providerDir, moduleName string, registerSkills bool, trackSkill func(qualifiedName string)) (*HostedObject, error) {
	m := GetPluginManager(ctx)
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	c := newMemoryCollectorContext(m, name, registerSkills, trackSkill)
	c.collector.setSource(hookSourceOf(name, filepath.Join(providerDir, "__init__.py")))
	drop := func(ctx context.Context) { c.collector.dropHooks(ctx, m) }
	drop(ctx)
	return host.LoadInstance(ctx, providerDir, moduleName, memoryProviderBase, memoryProviderCapture, c, drop)
}

// Loads a pip memory provider entry point in the plugin host as an instance, subclass, register module, factory or namespace
func CollectMemoryProviderEntryPoint(ctx context.Context, name string, registerSkills bool, trackSkill func(qualifiedName string)) (*HostedObject, error) {
	m := GetPluginManager(ctx)
	host, err := m.pluginHost()
	if err != nil {
		return nil, err
	}
	c := newMemoryCollectorContext(m, name, registerSkills, trackSkill)
	load := func(ctx context.Context) (map[string]any, error) {
		return host.instanceRef(ctx, memoryProviderCapture+":"+name, "load_memory_entry_point", map[string]any{
			"plugin_id": c.PluginID(), "name": name, "base": memoryProviderBase,
			"manifest": map[string]any{"name": name, "source": "entrypoint"},
		}, c)
	}
	drop := func(ctx context.Context) { c.collector.dropHooks(ctx, m) }
	return host.reloadableInstance(ctx, memoryProviderCapture, name, memoryProviderBase, load, drop)
}

// Imports a memory provider package or entry point in the plugin host ahead of construction, false when that import fails
func WarmMemoryProviderImport(ctx context.Context, name, providerDir, moduleName string, nativeWarm []string) bool {
	host, err := GetPluginManager(ctx).pluginHost()
	if err == nil {
		_, err = host.ensureStarted(ctx)
	}
	var root string
	if err == nil {
		root, err = hostPluginsRoot()
	}
	if err != nil {
		logDebug(fmt.Sprintf("memory provider '%s' warm-up import failed: %v", name, err))
		return false
	}
	params := map[string]any{"name": name, "path": providerDir, "module_name": moduleName, "parents": providerModuleParents,
		"plugins_root": root, "synthetic_namespace": nil, "native_warm": anyList(nativeWarm)}
	if providerDir != "" && !bundledMemoryProviderDir(providerDir) {
		params["synthetic_namespace"] = UserMemoryNamespace
	}
	raw, err := host.call(ctx, "warm_import", params, nil, 0, nil)
	if err != nil {
		logDebug(fmt.Sprintf("memory provider '%s' warm-up import failed: %v", name, err))
		return false
	}
	imported, _ := raw.(bool)
	return imported
}

// Imports a memory provider cli.py in the profile plugin host, reporting whether it has register_cli and a command handler
func DescribeMemoryProviderCLI(ctx context.Context, name, providerDir, moduleName string) (registerCLI, handler bool, err error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return false, false, err
	}
	if _, err := host.ensureStarted(ctx); err != nil {
		return false, false, err
	}
	raw, err := host.call(ctx, "describe_memory_cli", map[string]any{"name": name, "path": providerDir, "module_name": moduleName}, nil, 0, nil)
	if err != nil {
		return false, false, err
	}
	reply, _ := raw.(map[string]any)
	return reply["register_cli"] == true, reply["handler"] == true, nil
}

// Runs a memory provider argparse command in a fresh plugin host child on the given streams
func RunMemoryProviderCLI(ctx context.Context, name, providerDir, moduleName, description string, handler bool, argv []string, stdin io.Reader, stdout, stderr io.Writer) (rc int, dispatched bool, err error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return 0, false, err
	}
	params, err := pyJSONDumps(map[string]any{"name": name, "path": providerDir, "module_name": moduleName,
		"description": description, "handler": handler, "argv": anyList(argv)})
	if err != nil {
		return 0, false, err
	}
	outcome, err := os.CreateTemp("", "icebath-memory-cli-*.json")
	if err != nil {
		return 0, false, err
	}
	outcome.Close()
	defer os.Remove(outcome.Name())
	args, err := host.argv(ctx, "--run-memory-cli", string(params), outcome.Name())
	if err != nil {
		return 0, false, err
	}
	env, err := host.env()
	if err != nil {
		return 0, false, err
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env, cmd.Dir = env, host.home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	var exitErr *exec.ExitError
	if runErr := cmd.Run(); runErr != nil && !errors.As(runErr, &exitErr) {
		return 0, false, runErr
	}
	data, err := os.ReadFile(outcome.Name())
	if err != nil {
		return 0, false, err
	}
	parsed, err := pyJSONLoads(data)
	reply, ok := parsed.(map[string]any)
	if err != nil || !ok {
		return 0, false, fmt.Errorf("memory provider %s command exited %d without an outcome", config.PyRepr(name), exitCode(cmd.ProcessState))
	}
	code, _ := wireInt(reply["rc"])
	return int(code), reply["dispatched"] == true, nil
}

// Runs a user memory provider config_schema.py in the profile plugin host and returns its CONFIG_SCHEMA
func MemoryProviderConfigSchema(ctx context.Context, path string) (any, error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, err
	}
	return host.ConfigSchema(ctx, path)
}

// Whether the hosted method takes keyword, true for an unknown signature or one taking var keywords
func (o *HostedObject) AcceptsKeyword(method, keyword string) bool {
	sig := o.methods[method].Sig
	if sig == nil {
		return true
	}
	for _, p := range sig {
		if p.Kind == paramVarKeyword || (p.Name == keyword && (p.Kind == paramPositionalOrKeyword || p.Kind == paramKeywordOnly)) {
			return true
		}
	}
	return false
}

// Icebath function a hosted plugin receives as a callable that calls back into this process
type hostCallable struct {
	fn func(ctx context.Context, args []any, kwargs map[string]any) (any, error)
}

// Value the wire sends to the plugin host as a callable running fn here
func HostCallable(fn func(ctx context.Context, args []any, kwargs map[string]any) (any, error)) any {
	return &hostCallable{fn: fn}
}

// Registered parent callable and the child generation that holds it
type parentCallableSlot struct {
	fn         *hostCallable
	obj        HostObjectFacade
	event      weak.Pointer[HostEvent]
	generation int
}

// Registers a parent callable for the running child and answers its wire reference
func (h *PluginHost) parentRef(obj any) map[string]any {
	if e, ok := obj.(*HostEvent); ok && e != nil {
		return h.eventRef(e)
	}
	f, ok := obj.(*hostCallable)
	live, isObject := obj.(HostObjectFacade)
	if (!ok || f == nil) && (!isObject || live == nil) {
		return nil
	}
	h.mu.Lock()
	generation := h.generation
	h.mu.Unlock()
	h.stateMu.Lock()
	h.parentIDs++
	id := h.parentIDs
	if ok && f != nil {
		h.parents[id] = parentCallableSlot{fn: f, generation: generation}
		h.stateMu.Unlock()
		return map[string]any{"__parent_callable__": id}
	}
	h.parents[id] = parentCallableSlot{obj: live, generation: generation}
	h.stateMu.Unlock()
	return map[string]any{"__parent_object__": id, "type": live.PyTypeName()}
}

// Wire form of call arguments with parent callables registered for the child
func (h *PluginHost) encodeArgs(value any) any { return wireEncode(value, h.parentRef) }

// Runs one parent callable the child invoked and encodes its answer for the child
func (h *PluginHost) serveParentCall(ctx context.Context, params map[string]any) (any, error) {
	id, _ := wireInt(params["id"])
	h.stateMu.Lock()
	slot, ok := h.parents[id]
	h.stateMu.Unlock()
	if !ok || slot.fn == nil {
		return nil, &HostBuiltinError{Type: "LookupError", Message: fmt.Sprintf("icebath callable %d was released", id)}
	}
	resolve := func(ref map[string]any) (any, error) { return h.resolveRef(ref, "") }
	args, err := wireDecode(listOrEmpty(params["args"]), resolve)
	if err != nil {
		return nil, err
	}
	kwargs, err := wireDecode(mapOrEmpty(params["kwargs"]), resolve)
	if err != nil {
		return nil, err
	}
	result, err := slot.fn.fn(ctx, args.([]any), kwargs.(map[string]any))
	if err != nil {
		return nil, err
	}
	return h.encodeForHost(result), nil
}

// Forgets the parent callables the child released
func (h *PluginHost) releaseParentCallables(ids []any) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	for _, raw := range ids {
		if id, ok := wireInt(raw); ok && (h.parents[id].fn != nil || h.parents[id].obj != nil) {
			delete(h.parents, id)
		}
	}
}

// Forgets every parent callable held by a child of that generation or an earlier one
func (h *PluginHost) dropParentCallables(generation int) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	for id, slot := range h.parents {
		if slot.generation <= generation {
			delete(h.parents, id)
		}
	}
}

// Points every plugin host child at python until the returned restore runs, for other packages' tests
func SetHostInterpreterForTests(python string) (restore func()) {
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	return func() { hostInterpreter = previous }
}
