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

// Context managers of a provider module kept entered until released
type ProviderModuleHold struct{}

// Calls a memory provider submodule function with JSON text arguments, refused for every provider
func ProviderModuleJSONCall(ctx context.Context, pluginDir, moduleName, submodule, function, argsJSON string, pathArgs []int, require []string, track [][]any) (*ProviderModuleJSONResult, bool, error) {
	return nil, false, errPythonProviderModule
}

// Enters context managers of a memory provider submodule, refused for every provider
func HoldProviderModuleManagers(ctx context.Context, pluginDir, moduleName, submodule string, managers []ProviderModuleManager) (*ProviderModuleHold, bool, error) {
	return nil, false, errPythonProviderModule
}

// Leaves held context managers, refused for every provider
func (o *ProviderModuleHold) Release(ctx context.Context, bodyErrType, bodyErrMessage string) (suppressed bool, err error) {
	return false, errPythonProviderModule
}
