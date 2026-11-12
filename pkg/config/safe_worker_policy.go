package config

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Runtime components whose start forbids binding a worker policy afterwards
var runtimeImportNames = map[string]bool{
	"run_agent": true, "model_tools": true, "hermes_cli.config": true, "hermes_cli.env_loader": true,
	"hermes_cli.plugins": true, "providers": true, "agent.agent_init": true,
}

// Frozen bootstrap policy of an admission owned worker
type SafeWorkerPolicy struct {
	pid              int
	safeMode         bool
	ignoreUserConfig bool
	configJSON       string
}

func (p *SafeWorkerPolicy) PID() int               { return p.pid }
func (p *SafeWorkerPolicy) SafeMode() bool         { return p.safeMode }
func (p *SafeWorkerPolicy) IgnoreUserConfig() bool { return p.ignoreUserConfig }
func (p *SafeWorkerPolicy) ConfigJSON() string     { return p.configJSON }

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

// Python exception class the error stands for
func (e *WorkerPolicyError) PyTypeName() string { return e.Kind }

// Publishes the one policy of this process before any runtime component starts
func bindWorkerPolicy(safeMode, ignoreUserConfig bool, cfg map[string]any) (*SafeWorkerPolicy, error) {
	if cfg == nil {
		return nil, &WorkerPolicyError{"TypeError", "worker config must be a JSON object"}
	}
	serialized, err := pyjson.Dumps(orderedConfigJSON(cfg), pyjson.AllowNaN(false))
	if errors.Is(err, pyjson.ErrOutOfRange) || errors.Is(err, pyjson.ErrCircular) {
		return nil, &WorkerPolicyError{"ValueError", err.Error()}
	}
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

// Config tree as pyjson values with every mapping kept in its recorded key order
func orderedConfigJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		obj := pyjson.NewObject()
		for _, k := range MappingKeys(t) {
			obj.Set(k, orderedConfigJSON(t[k]))
		}
		return obj
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = orderedConfigJSON(item)
		}
		return out
	}
	return v
}

// Decoded JSON as config mappings in document key order with integers as Go ints
func ConfigFromJSON(v any) any {
	switch t := v.(type) {
	case *pyjson.Object:
		out := make(map[string]any, len(t.Keys))
		for _, k := range t.Keys {
			out[k] = ConfigFromJSON(t.Vals[k])
		}
		SetMappingKeys(out, t.Keys)
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

// Clears the process worker policy and runtime marks, the seam hermes tests reach by replacing _policy
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
	decoded, err := pyjson.Loads(policy.configJSON)
	if err != nil {
		panic("worker config snapshot does not decode: " + err.Error())
	}
	return ConfigFromJSON(decoded).(map[string]any)
}
