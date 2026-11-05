package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		return nil, errors.New("ordinary execution binds its config with BindWorkerConfig")
	}
	return bindWorkerPolicy(safeMode, true, cfg)
}

// Binds an ordinary managed worker frozen session config with plugins and providers loading as usual
func BindWorkerConfig(cfg map[string]any) (*SafeWorkerPolicy, error) {
	return bindWorkerPolicy(false, false, cfg)
}

// Publishes the one policy of this process before any runtime component starts
func bindWorkerPolicy(safeMode, ignoreUserConfig bool, cfg map[string]any) (*SafeWorkerPolicy, error) {
	if cfg == nil {
		return nil, errors.New("worker config must be a JSON object")
	}
	serialized, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	workerPolicyBind.Lock()
	defer workerPolicyBind.Unlock()
	if workerPolicy.Load() != nil {
		return nil, errors.New("worker policy is already bound")
	}
	if runtimeImported() {
		return nil, errors.New("worker policy must bind before runtime imports")
	}
	policy := &SafeWorkerPolicy{pid: os.Getpid(), safeMode: safeMode, ignoreUserConfig: ignoreUserConfig, configJSON: string(serialized)}
	workerPolicy.Store(policy)
	return policy, nil
}

// Bound policy of this process, panicking when it was inherited across a fork
func currentSafeWorkerPolicy() *SafeWorkerPolicy {
	policy := workerPolicy.Load()
	if policy != nil && policy.pid != os.Getpid() {
		panic("worker policy cannot cross a fork; use a fresh exec")
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
	dec := json.NewDecoder(bytes.NewReader([]byte(policy.configJSON)))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		panic("worker config snapshot does not decode: " + err.Error())
	}
	return pyJSONNumbers(out).(map[string]any)
}

// Value with JSON numbers as Python json gives them, int for integral literals and float otherwise
func pyJSONNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = pyJSONNumbers(val)
		}
	case []any:
		for i, val := range x {
			x[i] = pyJSONNumbers(val)
		}
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return int(n)
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	return v
}
