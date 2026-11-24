package config

import (
	"errors"
	"math"
	"os"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Runtime components whose start forbids binding a worker policy afterwards
var runtimeImportNames = map[string]bool{"config": true}

// Frozen bootstrap policy of an admission owned worker
type SafeWorkerPolicy struct {
	pid              int
	safeMode         bool
	ignoreUserConfig bool
	configJSON       string
}

var (
	workerPolicy     atomic.Pointer[SafeWorkerPolicy]
	workerPolicyBind sync.Mutex
	runtimeImportsMu sync.Mutex
	runtimeImports   = map[string]bool{}
)

// Records that a runtime component started, after which no worker policy may bind
func NoteRuntimeImport(name string) {
	runtimeImportsMu.Lock()
	runtimeImports[name] = true
	runtimeImportsMu.Unlock()
}

// Reports whether any runtime component that forbids a later binding has started
func runtimeImported() bool {
	runtimeImportsMu.Lock()
	defer runtimeImportsMu.Unlock()
	for name := range runtimeImports {
		if runtimeImportNames[name] {
			return true
		}
	}
	return false
}

// Binds a troubleshooting worker bypass that always ignores user config, refusing ordinary execution
func BindSafeWorkerPolicy(safeMode, ignoreUserConfig bool, cfg map[string]any) (*SafeWorkerPolicy, error) {
	if !safeMode && !ignoreUserConfig {
		return nil, &WorkerPolicyError{"ValueError", "ordinary execution binds its config with _bind_worker_config"}
	}
	return bindWorkerPolicy(safeMode, true, cfg)
}

// Binds an ordinary managed worker frozen session config with plugins and providers loading as usual
func BindWorkerConfig(cfg map[string]any) (*SafeWorkerPolicy, error) {
	return bindWorkerPolicy(false, false, cfg)
}

// TypeError, ValueError or RuntimeError a worker policy bind raises
type WorkerPolicyError struct{ Kind, Msg string }

func (e *WorkerPolicyError) Error() string { return e.Msg }

// Kind of refusal, TypeError, ValueError or RuntimeError
func (e *WorkerPolicyError) ErrorKind() string { return e.Kind }

// Publishes the one policy of this process before any runtime component starts
func bindWorkerPolicy(safeMode, ignoreUserConfig bool, cfg map[string]any) (*SafeWorkerPolicy, error) {
	if cfg == nil {
		return nil, &WorkerPolicyError{"TypeError", "worker config must be a JSON object"}
	}
	tree, err := orderedConfigJSON(cfg, map[any]bool{})
	if err != nil {
		return nil, &WorkerPolicyError{"ValueError", err.Error()}
	}
	if f, bad := nonFiniteFloat(tree); bad {
		return nil, &WorkerPolicyError{"ValueError", "worker config holds a non-finite number " + strconv.FormatFloat(f, 'g', -1, 64)}
	}
	serialized, err := jsonutil.Encode(tree)
	if err != nil {
		return nil, err
	}
	workerPolicyBind.Lock()
	defer workerPolicyBind.Unlock()
	if workerPolicy.Load() != nil {
		return nil, &WorkerPolicyError{"RuntimeError", "worker policy is already bound"}
	}
	if runtimeImported() {
		return nil, &WorkerPolicyError{"RuntimeError", "worker policy must bind before runtime imports"}
	}
	policy := &SafeWorkerPolicy{pid: os.Getpid(), safeMode: safeMode, ignoreUserConfig: ignoreUserConfig, configJSON: serialized}
	workerPolicy.Store(policy)
	return policy, nil
}

var errConfigCycle = errors.New("worker config holds a mapping or list that contains itself")

// Config tree as JSON values with every mapping kept in its recorded key order, refusing a container nested inside itself; active holds the containers on the current path
func orderedConfigJSON(v any, active map[any]bool) (any, error) {
	var id any
	switch t := v.(type) {
	case map[string]any:
		if len(t) > 0 {
			id = reflect.ValueOf(t).UnsafePointer()
		}
	case []any:
		if len(t) > 0 {
			id = &t[0]
		}
	}
	if id != nil {
		if active[id] {
			return nil, errConfigCycle
		}
		active[id] = true
		defer delete(active, id)
	}
	switch t := v.(type) {
	case map[string]any:
		obj := jsonutil.NewObject()
		for _, k := range MappingKeys(t) {
			item, err := orderedConfigJSON(t[k], active)
			if err != nil {
				return nil, err
			}
			obj.Set(k, item)
		}
		return obj, nil
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			conv, err := orderedConfigJSON(item, active)
			if err != nil {
				return nil, err
			}
			out[i] = conv
		}
		return out, nil
	}
	return v, nil
}

// First NaN or infinity anywhere in a converted config tree
func nonFiniteFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, math.IsNaN(x) || math.IsInf(x, 0)
	case float32:
		f := float64(x)
		return f, math.IsNaN(f) || math.IsInf(f, 0)
	case *jsonutil.Object:
		for _, it := range x.All() {
			if f, bad := nonFiniteFloat(it); bad {
				return f, true
			}
		}
	case []any:
		for _, it := range x {
			if f, bad := nonFiniteFloat(it); bad {
				return f, true
			}
		}
	}
	return 0, false
}

// Decoded JSON as config mappings in document key order with integers as Go ints
func ConfigFromJSON(v any) any {
	switch t := v.(type) {
	case *jsonutil.Object:
		out := make(map[string]any, t.Len())
		for _, k := range t.Keys() {
			out[k] = ConfigFromJSON(t.Value(k))
		}
		SetMappingKeys(out, t.Keys())
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = ConfigFromJSON(item)
		}
		return out
	case int64:
		if int64(int(t)) == t {
			return int(t)
		}
	}
	return v
}

// Clears the process worker policy and runtime marks, the seam tests reach by replacing policy
func ResetWorkerPolicyForTests() {
	workerPolicy.Store(nil)
	runtimeImportsMu.Lock()
	runtimeImports = map[string]bool{}
	runtimeImportsMu.Unlock()
}

// Bound policy of this process, panicking when it was inherited across a fork
func currentSafeWorkerPolicy() *SafeWorkerPolicy {
	policy := workerPolicy.Load()
	if policy != nil && policy.pid != os.Getpid() {
		panic(&WorkerPolicyError{"RuntimeError", "worker policy cannot cross a fork; use a fresh exec"})
	}
	return policy
}

// Bound worker policy, nil when the ordinary loader owns config
func CurrentSafeWorkerPolicy() *SafeWorkerPolicy { return currentSafeWorkerPolicy() }

// Reports whether a safe mode worker policy is bound
func SafeWorkerEnabled() bool {
	policy := currentSafeWorkerPolicy()
	return policy != nil && policy.safeMode
}

// Detached copy of the frozen worker config, nil when the ordinary loader owns config
func WorkerConfigSnapshot() map[string]any {
	policy := currentSafeWorkerPolicy()
	if policy == nil {
		return nil
	}
	decoded, err := jsonutil.Decode(policy.configJSON)
	if err != nil {
		panic("worker config snapshot does not decode: " + err.Error())
	}
	return ConfigFromJSON(decoded).(map[string]any)
}
