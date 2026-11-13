package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"weak"

	"github.com/nickheyer/icebath/pkg/config"
)

const memoryProviderBase = "agent.memory_provider:MemoryProvider"

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
		return hostCtxCall(ctx, h, c, method, args, kwargs)
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

// Loads the memory provider a guest plugin directory registers, its other registrations routed through a collector
func LoadGuestMemoryProvider(ctx context.Context, name, providerDir string, registerSkills bool, trackSkill func(qualifiedName string)) (*HostedObject, error) {
	m := GetPluginManager(ctx)
	mf, err := requireGuestManifest(providerDir)
	if err != nil {
		return nil, err
	}
	c := newMemoryCollectorContext(m, name, registerSkills, trackSkill)
	c.collector.setSource(hookSourceOf(name, filepath.Join(providerDir, filepath.FromSlash(mf.Entry[0]))))
	drop := func(ctx context.Context) { c.collector.dropHooks(ctx, m) }
	drop(ctx)
	inst, err := m.guestProviderLoad(mf, "memory", c, true, drop)
	if err != nil {
		return nil, err
	}
	return inst.object(ctx, memoryProviderBase)
}

// Settings schema a guest memory provider declared, nil when it declared none
func GuestConfigSchema(ctx context.Context, providerDir string) (map[string]any, error) {
	mf, err := requireGuestManifest(providerDir)
	if err != nil {
		return nil, err
	}
	host, err := GetPluginManager(ctx).categoryGuestHost(mf)
	if err != nil {
		return nil, err
	}
	return host.ConfigSchema(ctx)
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
	if o, ok := obj.(*HostedObject); ok && o != nil && o.slot != nil {
		return h.childObjectRef(o)
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
