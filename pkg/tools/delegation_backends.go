package tools

import (
	"context"
	"slices"
	"sync"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	delegationRebuildMu       sync.Mutex
	delegationRebuildRequests = map[string]bool{}
)

// Wiring the provider, mcp and observability lanes install, each panics when unassigned
var DelegationBackendHooks struct {
	ListProviders        func(ctx context.Context) []DelegationProvider
	RefreshAgentMCPTools func(ctx context.Context, a DelegationRebuildAgent, quietMode, contentAware bool)
	RecordCacheBreak     func(ctx context.Context, a DelegationRebuildAgent, reason string)
}

// One selectable delegation backend, Available false carrying the reason it cannot run
type BackendInfo struct {
	Name        string
	Label       string
	Description string
	Available   bool
	Reason      string
}

// Refusal of a backend switch, Kind naming the Python exception class
type DelegationBackendError struct{ Kind, Message string }

func (e *DelegationBackendError) Error() string { return e.Message }

// Agent state recording which backend its delegate_task definition was built from
type DelegationBackendAgent interface {
	Tools() []map[string]any
	DelegationBackend() (backend string, set bool)
	SetDelegationBackend(backend string)
}

// Agent surface a turn start rebuild reads and refreshes
type DelegationRebuildAgent interface {
	DelegationBackendAgent
	TurnOrigin() string
	DelegateDepth() int
	SessionID() string
	SessionEnv() *Env
}

func delegationBackendInfo(ctx context.Context, name string) BackendInfo {
	p, reason := DelegationBackendStatus(ctx, name)
	if p == nil {
		return BackendInfo{Name: name, Label: name, Available: false, Reason: reason}
	}
	return BackendInfo{Name: name, Label: p.DisplayName(), Description: p.Summary(), Available: reason == "", Reason: reason}
}

// Default backend first, then every registered backend with unavailable ones carrying their reason
func ListDelegationBackends(ctx context.Context) []BackendInfo {
	seen := map[string]bool{}
	var others []string
	for _, p := range DelegationBackendHooks.ListProviders(ctx) {
		if name := p.Name(); name != DefaultDelegationBackend && !seen[name] {
			seen[name] = true
			others = append(others, name)
		}
	}
	slices.Sort(others)
	out := []BackendInfo{delegationBackendInfo(ctx, DefaultDelegationBackend)}
	for _, name := range others {
		out = append(out, delegationBackendInfo(ctx, name))
	}
	return out
}

// Backend config.yaml selects now, what a tool build reads
func SavedDelegationBackend(ctx context.Context) string {
	return ConfiguredBackend(LoadDelegationConfig(ctx))
}

func delegateDefinition(tools []map[string]any) map[string]any {
	for _, t := range tools {
		if fn, _ := t["function"].(map[string]any); fn["name"] == "delegate_task" {
			return t
		}
	}
	return nil
}

// Records the backend as the session's when the agent carries the delegate_task the definitions hold
func AdoptBuiltBackend(a DelegationBackendAgent, defs []map[string]any, backend string) {
	if pyrepr.Equal(delegateDefinition(a.Tools()), delegateDefinition(defs)) {
		a.SetDelegationBackend(backend)
	}
}

// Backend the agent delegate_task was built from, empty for no agent or one never built
func PinnedBackend(a DelegationBackendAgent) string {
	if a == nil {
		return ""
	}
	backend, _ := a.DelegationBackend()
	return backend
}

// Persists delegation.backend to the profile config, queueing a next turn rebuild of the session when asked
func SetGlobalBackend(ctx context.Context, name, sessionKey string, applyNow bool) error {
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		return &DelegationBackendError{Kind: "PermissionError", Message: config.FormatManagedMessage(ctx, h, "set delegation.backend")}
	}
	if applyNow && sessionKey == "" {
		return &DelegationBackendError{Kind: "ValueError", Message: "No session to apply the switch to: apply_now needs the command's session_key."}
	}
	key := pystr.Lower(pystr.Strip(name))
	if info := delegationBackendInfo(ctx, key); !info.Available {
		return &DelegationBackendError{Kind: "ValueError", Message: info.Reason}
	}
	path := home.ConfigPath(h)
	userConfig, err := config.ReadUserConfigRaw(ctx, path)
	if err != nil {
		return err
	}
	if userConfig == nil {
		userConfig = map[string]any{}
	}
	section, ok := userConfig["delegation"].(map[string]any)
	if !ok {
		section = map[string]any{}
		userConfig["delegation"] = section
	}
	section["backend"] = key
	if err := config.AtomicConfigWrite(ctx, h, path, userConfig, ""); err != nil {
		return err
	}
	if applyNow {
		delegationRebuildMu.Lock()
		delegationRebuildRequests[sessionKey] = true
		delegationRebuildMu.Unlock()
	}
	return nil
}

func currentDelegationSessionKey(a DelegationRebuildAgent) string {
	if key := sessionEnvValue(a.SessionEnv(), "ICEBATH_SESSION_KEY"); key != "" {
		return key
	}
	return a.SessionID()
}

// Rebuilds the session tools at turn start when a switch asked for this session, true when the tool list changed
func ApplyRequestedToolRebuild(ctx context.Context, a DelegationRebuildAgent) bool {
	if a.TurnOrigin() != "" || a.DelegateDepth() != 0 {
		return false
	}
	key := currentDelegationSessionKey(a)
	delegationRebuildMu.Lock()
	if !delegationRebuildRequests[key] {
		delegationRebuildMu.Unlock()
		return false
	}
	delete(delegationRebuildRequests, key)
	delegationRebuildMu.Unlock()
	before := a.Tools()
	DelegationBackendHooks.RefreshAgentMCPTools(ctx, a, true, true)
	after := a.Tools()
	if unsafe.SliceData(after) == unsafe.SliceData(before) && len(after) == len(before) {
		return false
	}
	DelegationBackendHooks.RecordCacheBreak(ctx, a, "toolset_change")
	return true
}

// Plugin facing backend API as plain data
type DelegationBackends struct{}

// Every backend as a field mapping
func (DelegationBackends) ListBackends(ctx context.Context) []map[string]any {
	infos := ListDelegationBackends(ctx)
	out := make([]map[string]any, len(infos))
	for i, info := range infos {
		out[i] = map[string]any{"name": info.Name, "label": info.Label, "description": info.Description,
			"available": info.Available, "reason": info.Reason}
	}
	return out
}

// Saves the backend choice for the profile
func (DelegationBackends) SetGlobalBackend(ctx context.Context, name, sessionKey string, applyNow bool) error {
	return SetGlobalBackend(ctx, name, sessionKey, applyNow)
}
