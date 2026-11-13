package plugins

import (
	"context"
	"sort"
	"time"
)

// Builders of a hosted call's positional arguments and how its answer crosses back
type HostedCallOptions struct {
	// Per positional argument, the dotted builder path the child makes the Python value with from the dict sent
	Build []string
	// Base an instance answer is judged against when it crosses as a hosted object
	Describe string
	// Per positional argument, the method whose answer the child returns after the call
	Dump []string
	// Per positional argument, an item of another argument's attribute passed in its place
	Derive []*HostedDerive
	// Whether the call runs with the user's terminal attached to the child's standard streams
	Terminal bool
	// Longest time the call may run, zero for no limit
	Timeout time.Duration
}

// Argument taken as getattr(args[From], Attr)[Key], the key built by KeyBuild when set
type HostedDerive struct {
	From     int
	Attr     string
	Key      any
	KeyBuild string
}

// Calls the plugin callable with built arguments, answering the value and the dumped arguments
func (f *HostedCallable) CallWith(ctx context.Context, args []any, kwargs map[string]any, opts HostedCallOptions) (any, []any, error) {
	params := map[string]any{"ref": f.ref, "args": f.host.encodeArgs(nonNilArgs(args)), "kwargs": f.host.encodeArgs(nonNilKwargs(kwargs)),
		"build": anyList(opts.Build), "describe": opts.Describe, "dump": anyList(opts.Dump), "terminal": opts.Terminal}
	if len(opts.Derive) > 0 {
		derive := make([]any, len(opts.Derive))
		for i, d := range opts.Derive {
			if d != nil {
				derive[i] = map[string]any{"from": int64(d.From), "attr": d.Attr, "key": d.Key, "key_build": d.KeyBuild}
			}
		}
		params["derive"] = derive
	}
	raw, err := f.host.callIn(ctx, opts.Describe, "invoke", params, &f.generation, opts.Timeout, nil)
	if err != nil || len(opts.Dump) == 0 {
		return raw, nil, err
	}
	reply, _ := raw.(map[string]any)
	dumped, _ := reply["dumped"].([]any)
	return reply["value"], dumped, nil
}

// Calls one method of the plugin object with positional arguments built in the child by the named builders
func (o *HostedObject) CallBuilt(ctx context.Context, method string, args []any, kwargs map[string]any, build []string) (any, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	return o.host.callIn(ctx, o.liveClientBase(), "obj_invoke", map[string]any{"ref": ref, "method": method, "args": o.host.encodeArgs(nonNilArgs(args)),
		"kwargs": o.host.encodeArgs(nonNilKwargs(kwargs)), "build": anyList(build)}, &generation, 0, declaredReturn(o.Base, method))
}

// Python names among those asked that the object's class defines outside the module or the instance holds
func (o *HostedObject) DefinedOutside(ctx context.Context, module string, names []string) (map[string]bool, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := o.host.call(ctx, "obj_defined_outside", map[string]any{"ref": ref, "module": module, "names": anyList(names)}, &generation, 0, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range listOrEmpty(raw) {
		out[wireStr(name)] = true
	}
	return out, nil
}

// Routes a hosted platform adapter's inbound events and its own lifecycle hook calls to the Go adapter
func (o *HostedObject) BindPlatform(ctx context.Context, installHandle bool, handle, runner func(ctx context.Context, event map[string]any) (any, error),
	lifecycle func(ctx context.Context, name string, args []any, kwargs map[string]any) error) error {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return err
	}
	wrap := func(fn func(ctx context.Context, event map[string]any) (any, error)) any {
		return HostCallable(func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			var event map[string]any
			if len(args) > 0 {
				event = argMap(args[0])
			}
			return fn(ctx, event)
		})
	}
	_, err = o.host.call(ctx, "platform_bind", map[string]any{"ref": ref, "install_handle": installHandle,
		"args": o.host.encodeArgs([]any{wrap(handle), wrap(runner), HostCallable(func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			if len(args) < 3 {
				return nil, &HostBuiltinError{Type: "TypeError", Message: "lifecycle bridge takes a name, arguments and keywords"}
			}
			return nil, lifecycle(ctx, wireStr(args[0]), listOrEmpty(args[1]), argMap(args[2]))
		})})}, &generation, 0, nil)
	return err
}

// Context the plugin host was started for, its profile home bound
func (f *HostedCallable) HostContext() context.Context { return f.host.baseCtx }

// How a hosted platform adapter call crosses, its arguments built in the child, an attribute read, or the answer held there
type PlatformCallOptions struct {
	// Per positional argument, the builder the child makes the Python value with, a leading ** passing the dict as keywords
	Build []string
	// Per keyword argument, the builder the child makes the Python value with when the value is not None
	BuildKw map[string]string
	// Whether the name is read as an attribute instead of called
	Get bool
	// Upstream class a non None answer stays in the child as, crossing as a hosted object, empty to encode it
	Hold string
	// Whether the class implementation runs past the instance bridge, answering None when the class has none
	Own bool
}

// Calls or reads one name of the adapter surface on the hosted object, private overridable names included
func (o *HostedObject) PlatformCall(ctx context.Context, name string, args []any, kwargs map[string]any, opts PlatformCallOptions) (any, error) {
	ref, generation, err := o.addr(ctx)
	if err != nil {
		return nil, err
	}
	return o.host.callIn(ctx, opts.Hold, "platform_call", map[string]any{"ref": ref, "name": name, "get": opts.Get, "hold": opts.Hold != "", "own": opts.Own,
		"args": o.host.encodeArgs(nonNilArgs(args)), "kwargs": o.host.encodeArgs(nonNilKwargs(kwargs)), "build": anyList(opts.Build), "build_kw": wireStrings(opts.BuildKw)}, &generation, 0, nil)
}

// Wire form of a string map
func wireStrings(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Wire reference of an object living in this child, marked stale when it belongs to another host or an earlier child
func (h *PluginHost) childObjectRef(o *HostedObject) map[string]any {
	s := o.slot
	s.mu.Lock()
	ref, generation, owner := s.ref, s.generation, s.host
	s.mu.Unlock()
	h.mu.Lock()
	current := h.generation
	h.mu.Unlock()
	out := map[string]any{"__child_object__": ref}
	if owner != h || generation != current {
		out["stale"] = true
	}
	return out
}

// Dict value the child rebuilds with its keys in the given order, keys absent from values skipped
func OrderedDict(keys []string, values map[string]any) map[string]any {
	items := make([]any, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, k := range keys {
		if v, ok := values[k]; ok && !seen[k] {
			items = append(items, []any{k, v})
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(values))
	for k := range values {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		items = append(items, []any{k, values[k]})
	}
	return map[string]any{"__dict_items__": items}
}
