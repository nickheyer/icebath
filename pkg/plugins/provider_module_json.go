package plugins

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Answer of a JSON argument call into a provider module, one JSON text, the paths sharing an object, tracked dict keys holding the result
type ProviderModuleJSONResult struct {
	RootJSON     *string
	Shared       [][][]any
	ResultKeys   [][]string
	ResultIsPath bool
	ResultPath   string
}

// Arguments after the call, result and tracked argument parts, containers that were one object in the child one object here
func (r *ProviderModuleJSONResult) Graph() (args []any, result any, tracked []any, err error) {
	if r.RootJSON == nil {
		return nil, nil, nil, &HostBuiltinError{Type: "TypeError", Message: "provider module values are not JSON serializable"}
	}
	parsed, err := pyjson.Loads(*r.RootJSON)
	if err != nil {
		return nil, nil, nil, err
	}
	root, _ := parsed.(*pyjson.Object)
	if root == nil {
		return nil, nil, nil, fmt.Errorf("plugin host: provider_module_json_call answered %T instead of a root object", parsed)
	}
	groups := append([][][]any(nil), r.Shared...)
	sort.SliceStable(groups, func(i, j int) bool { return len(groups[i][0]) < len(groups[j][0]) })
	for _, group := range groups {
		shared, ok := graphAt(root, group[0])
		if !ok {
			continue
		}
		for _, path := range group[1:] {
			graphSet(root, path, shared)
		}
	}
	args, _ = root.GetOr("a", nil).([]any)
	tracked, _ = root.GetOr("t", nil).([]any)
	return args, root.GetOr("r", nil), tracked, nil
}

// Object key or list index a wire path element names
func graphKey(key any) (string, int) {
	switch k := key.(type) {
	case string:
		return k, -1
	case int64:
		return strconv.FormatInt(k, 10), int(k)
	case float64:
		return strconv.FormatInt(int64(k), 10), int(k)
	case bool:
		if k {
			return "true", -1
		}
		return "false", -1
	case nil:
		return "null", -1
	}
	return fmt.Sprint(key), -1
}

// Value at a key path of a parsed JSON tree
func graphAt(node any, path []any) (any, bool) {
	for _, key := range path {
		name, index := graphKey(key)
		switch t := node.(type) {
		case *pyjson.Object:
			v, ok := t.Get(name)
			if !ok {
				return nil, false
			}
			node = v
		case []any:
			if index < 0 || index >= len(t) {
				return nil, false
			}
			node = t[index]
		default:
			return nil, false
		}
	}
	return node, true
}

// Replaces the value at a key path of a parsed JSON tree
func graphSet(root any, path []any, value any) {
	if len(path) == 0 {
		return
	}
	parent, ok := graphAt(root, path[:len(path)-1])
	if !ok {
		return
	}
	name, index := graphKey(path[len(path)-1])
	switch t := parent.(type) {
	case *pyjson.Object:
		if _, has := t.Get(name); has {
			t.Set(name, value)
		}
	case []any:
		if index >= 0 && index < len(t) {
			t[index] = value
		}
	}
}

// One context manager of a provider module, a module attribute or the result of calling one
type ProviderModuleManager struct {
	Name     string
	Call     bool
	ArgsJSON string
	PathArgs []int
}

// Context managers of a provider module kept entered in the child until released
type ProviderModuleHold struct {
	host       *PluginHost
	ref        int64
	generation int
}

// Positions as a wire list
func wirePositions(positions []int) []any {
	out := make([]any, len(positions))
	for i, p := range positions {
		out[i] = p
	}
	return out
}

// Calls a provider module function with JSON text arguments in the child, loaded false when the package failed to import
func (h *PluginHost) ProviderModuleJSONCall(ctx context.Context, pluginDir, moduleName, submodule, function, argsJSON string, pathArgs []int, require []string, track [][]any) (*ProviderModuleJSONResult, bool, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, false, err
	}
	params, err := providerModuleParams(pluginDir, moduleName, submodule)
	if err != nil {
		return nil, false, err
	}
	required := make([]any, len(require))
	for i, name := range require {
		required[i] = name
	}
	tracked := make([]any, len(track))
	for i, path := range track {
		tracked[i] = path
	}
	params["function"], params["args_json"], params["path_args"], params["require"], params["track"] = function, argsJSON, wirePositions(pathArgs), required, tracked
	h.mu.Lock()
	ch := h.channel
	h.mu.Unlock()
	if ch == nil {
		return nil, false, h.unavailableAsExit(&PluginHostUnavailable{Reason: "not started"})
	}
	raw, err := ch.Call(ctx, "provider_module_json_call", params, 0)
	if err != nil {
		return nil, true, h.unavailableAsExit(err)
	}
	reply, _ := raw.(map[string]any)
	if loaded, _ := reply["loaded"].(bool); !loaded {
		return nil, false, nil
	}
	out := &ProviderModuleJSONResult{}
	if text, ok := reply["root_json"].(string); ok {
		out.RootJSON = &text
	}
	out.ResultIsPath, _ = reply["result_is_path"].(bool)
	out.ResultPath, _ = reply["result_path"].(string)
	for _, keys := range listOrEmpty(reply["result_keys"]) {
		names := []string{}
		for _, key := range listOrEmpty(keys) {
			name, _ := graphKey(key)
			names = append(names, name)
		}
		out.ResultKeys = append(out.ResultKeys, names)
	}
	for _, group := range listOrEmpty(reply["shared"]) {
		var paths [][]any
		for _, path := range listOrEmpty(group) {
			paths = append(paths, listOrEmpty(path))
		}
		if len(paths) > 1 {
			out.Shared = append(out.Shared, paths)
		}
	}
	return out, true, nil
}

// Enters context managers of a provider module in order on one child thread, loaded false when the package failed to import
func (h *PluginHost) HoldProviderModuleManagers(ctx context.Context, pluginDir, moduleName, submodule string, managers []ProviderModuleManager) (*ProviderModuleHold, bool, error) {
	if _, err := h.ensureStarted(ctx); err != nil {
		return nil, false, err
	}
	params, err := providerModuleParams(pluginDir, moduleName, submodule)
	if err != nil {
		return nil, false, err
	}
	specs := make([]any, len(managers))
	for i, m := range managers {
		specs[i] = map[string]any{"name": m.Name, "call": m.Call, "args_json": m.ArgsJSON, "path_args": wirePositions(m.PathArgs)}
	}
	params["managers"] = specs
	h.mu.Lock()
	ch := h.channel
	generation := h.generation
	h.mu.Unlock()
	if ch == nil {
		return nil, false, h.unavailableAsExit(&PluginHostUnavailable{Reason: "not started"})
	}
	raw, err := ch.Call(ctx, "provider_module_hold", params, 0)
	if err != nil {
		return nil, true, h.unavailableAsExit(err)
	}
	reply, _ := raw.(map[string]any)
	if loaded, _ := reply["loaded"].(bool); !loaded {
		return nil, false, nil
	}
	ref, ok := wireInt(reply["hold"])
	if !ok {
		return nil, true, fmt.Errorf("plugin host: provider_module_hold answered %T instead of a hold", reply["hold"])
	}
	return &ProviderModuleHold{host: h, ref: ref, generation: generation}, true, nil
}

// Leaves the held context managers in reverse order, a body failure raised inside them first as the named Python class
func (o *ProviderModuleHold) Release(ctx context.Context, bodyErrType, bodyErrMessage string) (suppressed bool, err error) {
	h := o.host
	h.mu.Lock()
	ch := h.channel
	current := h.generation
	h.mu.Unlock()
	if ch == nil {
		return false, h.unavailableAsExit(&PluginHostUnavailable{Reason: "not started"})
	}
	if current != o.generation {
		return false, h.unavailableAsExit(&PluginHostUnavailable{Reason: "this object belonged to a plugin host process that has exited"})
	}
	params := map[string]any{"hold": o.ref, "error": nil}
	if bodyErrType != "" {
		params["error"] = map[string]any{"type": bodyErrType, "message": bodyErrMessage}
	}
	raw, err := ch.Call(context.WithoutCancel(ctx), "provider_module_release", params, 0)
	if err != nil {
		return false, h.unavailableAsExit(err)
	}
	reply, _ := raw.(map[string]any)
	suppressed, _ = reply["suppressed"].(bool)
	return suppressed, nil
}

// Calls a provider module function with JSON text arguments in the plugin host of the context home
func ProviderModuleJSONCall(ctx context.Context, pluginDir, moduleName, submodule, function, argsJSON string, pathArgs []int, require []string, track [][]any) (*ProviderModuleJSONResult, bool, error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, false, err
	}
	return host.ProviderModuleJSONCall(ctx, pluginDir, moduleName, submodule, function, argsJSON, pathArgs, require, track)
}

// Enters context managers of a provider module in the plugin host of the context home
func HoldProviderModuleManagers(ctx context.Context, pluginDir, moduleName, submodule string, managers []ProviderModuleManager) (*ProviderModuleHold, bool, error) {
	host, err := GetPluginManager(ctx).pluginHost()
	if err != nil {
		return nil, false, err
	}
	return host.HoldProviderModuleManagers(ctx, pluginDir, moduleName, submodule, managers)
}
