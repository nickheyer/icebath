package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Loaded hook handler taking the event type and its context, returning a decision or nil
type hookHandler func(ctx context.Context, eventType string, hookContext map[string]any) (any, error)

// Python TypeError carrying only its message
type pyTypeError struct{ msg string }

func (e *pyTypeError) Error() string { return e.msg }

// Exception class the error reports, TypeError
func (e *pyTypeError) PyTypeName() string { return "TypeError" }

// Hooks dir of the active profile home
func resolveHooksDir(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "hooks")
}

// Prints why a hook dir was skipped
func skipHook(name, reason string) {
	fmt.Fprintf(os.Stdout, "[hooks] Skipping %s: %s\n", name, reason)
}

// One valid hook dir with its name, events, handler and description
type loadedHookDir struct {
	name        string
	events      any
	handle      hookHandler
	description any
}

// Hook of a valid dir with its handler imported in the plugin host, nil with the reason printed otherwise
func loadHookDir(ctx context.Context, hookDir string) (*loadedHookDir, error) {
	manifestPath, handlerPath := filepath.Join(hookDir, "HOOK.yaml"), filepath.Join(hookDir, "handler.py")
	if !pathExists(manifestPath) || !pathExists(handlerPath) {
		return nil, nil
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	text, err := pystr.DecodeUTF8SigFile(raw)
	if err != nil {
		return nil, err
	}
	doc, err := config.SafeLoad(text)
	if err != nil {
		return nil, err
	}
	manifest, ok := doc.(map[string]any)
	if !pyrepr.Truthy(doc) || !ok {
		skipHook(filepath.Base(hookDir), "invalid HOOK.yaml")
		return nil, nil
	}
	hookName := filepath.Base(hookDir)
	if v, ok := manifest["name"]; ok {
		hookName = pyrepr.Str(v)
	}
	var events any = []any{}
	if v, ok := manifest["events"]; ok {
		events = v
	}
	if !pyrepr.Truthy(events) {
		skipHook(hookName, "no events declared")
		return nil, nil
	}
	handle, reason, err := plugins.LoadGatewayHook(ctx, hookName, handlerPath)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		skipHook(hookName, reason)
		return nil, nil
	}
	var description any = ""
	if v, ok := manifest["description"]; ok {
		description = v
	}
	return &loadedHookDir{name: hookName, events: events, handle: hostedHookHandler(handle), description: description}, nil
}

// Handler calling the hosted handle, a TypeError on every call when the attribute is not callable
func hostedHookHandler(handle any) hookHandler {
	fn, ok := handle.(*plugins.HostedCallable)
	if !ok {
		return func(context.Context, string, map[string]any) (any, error) {
			return nil, &pyTypeError{msg: fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(handle))}
		}
	}
	return func(ctx context.Context, eventType string, hookContext map[string]any) (any, error) {
		return fn.Call(ctx, []any{eventType, hookContext}, nil)
	}
}

// Whether the path exists
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Event names a manifest events value iterates to, a TypeError for a value Python cannot iterate
func hookEventNames(events any) ([]string, error) {
	switch v := events.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			out = append(out, pyrepr.Str(e))
		}
		return out, nil
	case string:
		out := make([]string, 0, len(v))
		for _, r := range v {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		out := make([]string, 0, len(v))
		for k := range v {
			out = append(out, k)
		}
		sort.Strings(out)
		return out, nil
	}
	return nil, &pyTypeError{msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(events))}
}

// Discovers, loads and fires the event hooks of one home
type HookRegistry struct {
	handlers    map[string][]hookHandler
	loadedHooks []map[string]any
}

// Registry with no hooks loaded
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{handlers: map[string][]hookHandler{}}
}

// Metadata of every loaded hook
func (h *HookRegistry) LoadedHooks() []map[string]any {
	return append([]map[string]any{}, h.loadedHooks...)
}

// Registers the always on built in hooks, of which none ship
func (h *HookRegistry) registerBuiltinHooks() {}

// Registers the built in hooks then loads every valid hook dir under the active profile's hooks dir
func (h *HookRegistry) discoverAndLoad(ctx context.Context) error {
	h.registerBuiltinHooks()
	hooksDir := resolveHooksDir(ctx)
	if !pathExists(hooksDir) {
		return nil
	}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		hookDir := filepath.Join(hooksDir, entry.Name())
		if info, err := os.Stat(hookDir); err != nil || !info.IsDir() {
			continue
		}
		loaded, err := func() (loaded *loadedHookDir, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return loadHookDir(ctx, hookDir)
		}()
		if err != nil {
			fmt.Fprintf(os.Stdout, "[hooks] Error loading hook %s: %s\n", entry.Name(), pyrepr.ExceptionMessage(err))
			continue
		}
		if loaded == nil {
			continue
		}
		names, err := hookEventNames(loaded.events)
		if err != nil {
			return err
		}
		for _, event := range names {
			h.handlers[event] = append(h.handlers[event], loaded.handle)
		}
		h.loadedHooks = append(h.loadedHooks, map[string]any{"name": loaded.name, "description": loaded.description,
			"events": loaded.events, "path": hookDir})
		fmt.Fprintf(os.Stdout, "[hooks] Loaded hook '%s' for events: %s\n", loaded.name, pyrepr.Str(loaded.events))
	}
	return nil
}

// Exact match handlers first, then the base wildcard handlers of a namespaced event
func (h *HookRegistry) resolveHandlers(eventType string) []hookHandler {
	handlers := append([]hookHandler(nil), h.handlers[eventType]...)
	if base, _, found := strings.Cut(eventType, ":"); found {
		handlers = append(handlers, h.handlers[base+":*"]...)
	}
	return handlers
}

// Fires every handler of the event, discarding their return values
func (h *HookRegistry) emit(ctx context.Context, eventType string, hookContext map[string]any) {
	h.emitCollect(ctx, eventType, hookContext)
}

// Fires the handlers in order and returns their non nil results, printing a failing handler's error
func (h *HookRegistry) emitCollect(ctx context.Context, eventType string, hookContext map[string]any) []any {
	if hookContext == nil {
		hookContext = map[string]any{}
	}
	results := []any{}
	for _, fn := range h.resolveHandlers(eventType) {
		result, err := func() (result any, err error) {
			defer func() {
				if rec := recover(); rec != nil {
					err = fmt.Errorf("%v", rec)
				}
			}()
			return fn(ctx, eventType, hookContext)
		}()
		if err != nil {
			fmt.Fprintf(os.Stdout, "[hooks] Error in handler for '%s': %s\n", eventType, pyrepr.ExceptionMessage(err))
			continue
		}
		if result != nil {
			results = append(results, result)
		}
	}
	return results
}

// Hook registry per served profile home, picked at emit time from the active home
type ProfileHookRegistries struct {
	byHome map[string]*HookRegistry
	lock   sync.Mutex
}

// Registries with no home loaded yet
func NewProfileHookRegistries() *ProfileHookRegistries {
	return &ProfileHookRegistries{byHome: map[string]*HookRegistry{}}
}

// Registry of the active home, discovered and loaded inside its scope on first use
func (p *ProfileHookRegistries) active(ctx context.Context) (*HookRegistry, error) {
	key := home.Key(home.Dir(ctx))
	p.lock.Lock()
	defer p.lock.Unlock()
	if registry := p.byHome[key]; registry != nil {
		return registry, nil
	}
	registry := NewHookRegistry()
	if err := registry.discoverAndLoad(ctx); err != nil {
		return nil, err
	}
	p.byHome[key] = registry
	return registry, nil
}

// Metadata of every hook loaded for the active home
func (p *ProfileHookRegistries) LoadedHooks(ctx context.Context) ([]map[string]any, error) {
	registry, err := p.active(ctx)
	if err != nil {
		return nil, err
	}
	return registry.LoadedHooks(), nil
}

// Loads the active home's hooks now
func (p *ProfileHookRegistries) discoverAndLoad(ctx context.Context) error {
	_, err := p.active(ctx)
	return err
}

// Fires the active home's handlers of the event
func (p *ProfileHookRegistries) emit(ctx context.Context, eventType string, hookContext map[string]any) error {
	registry, err := p.active(ctx)
	if err != nil {
		return err
	}
	registry.emit(ctx, eventType, hookContext)
	return nil
}

// Fires the active home's handlers of the event and returns their non nil results
func (p *ProfileHookRegistries) emitCollect(ctx context.Context, eventType string, hookContext map[string]any) ([]any, error) {
	registry, err := p.active(ctx)
	if err != nil {
		return nil, err
	}
	return registry.emitCollect(ctx, eventType, hookContext), nil
}
