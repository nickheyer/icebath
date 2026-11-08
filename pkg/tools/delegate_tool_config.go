package tools

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	DefaultMaxConcurrentChildren   = 10
	DelegationMaxDepth             = 1
	minSpawnDepth                  = 1
	defaultOneshotMaxChildren      = 2
	delegationChildTimeoutFloorSec = 30.0
)

var delegateToolLog = slog.Default().With("logger", "tools.delegate_tool")

var highConcurrencyWarned, legacyMaxAsyncWarned atomic.Bool

// Surfaces with no human chat thread, matching gateway session_context NON_MESSAGING_SESSION_SURFACES
var NonMessagingSessionSurfaces = map[string]bool{
	"": true, "api_server": true, "cli": true, "codex": true, "desktop": true, "gateway": true, "kanban": true,
	"local": true, "msgraph_webhook": true, "tool": true, "tui": true, "webhook": true,
}

// Legacy cli CLI_CONFIG loader the cli_config_load port installs, read only under ICEBATH_IGNORE_USER_CONFIG=1
var LoadCLIConfig func(ctx context.Context) map[string]any

// Delegation section of the effective config, the legacy CLI config when user config is ignored, empty on failure
func LoadDelegationConfig(ctx context.Context) map[string]any {
	if os.Getenv("ICEBATH_IGNORE_USER_CONFIG") != "1" {
		h := home.Dir(ctx)
		if tree, err := config.LoadReadonly(home.WithOverride(ctx, h), h, config.NewSecrets(h).Lookup()); err == nil {
			raw := tree["delegation"]
			if raw == nil || !pyrepr.Truthy(raw) {
				return map[string]any{}
			}
			if cfg, ok := raw.(map[string]any); ok {
				return cfg
			}
		}
	}
	if raw := LoadCLIConfig(ctx)["delegation"]; raw != nil && pyrepr.Truthy(raw) {
		if cfg, ok := raw.(map[string]any); ok {
			return cfg
		}
	}
	return map[string]any{}
}

func init() {
	DelegationConfig = func(env *Env) map[string]any { return LoadDelegationConfig(delegationEnvContext(env)) }
}

// Context carrying the home a dynamic schema build reads its delegation limits from
func delegationEnvContext(env *Env) context.Context {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return ctx
}

// Dangerous command refusal a subagent sees by default, logged for audit
func subagentAutoDeny(command, description string, _ map[string]any) string {
	delegateToolLog.Warn(fmt.Sprintf("Subagent auto-denied dangerous command: %s (%s). Set delegation.subagent_auto_approve: true to allow.", command, description))
	return "deny"
}

// One time approval a subagent gets under delegation.subagent_auto_approve, logged for audit
func subagentAutoApprove(command, description string, _ map[string]any) string {
	delegateToolLog.Warn(fmt.Sprintf("Subagent auto-approved dangerous command: %s (%s)", command, description))
	return "once"
}

// Non interactive approval callback for subagent workers chosen by delegation.subagent_auto_approve
func SubagentApprovalCallback(ctx context.Context) func(command, description string, kwargs map[string]any) string {
	v, ok := LoadDelegationConfig(ctx)["subagent_auto_approve"]
	if !ok {
		v = false
	}
	if isTruthyValue(v, false) {
		return subagentAutoApprove
	}
	return subagentAutoDeny
}

// Config value over env value over default, a bad config value warned and defaulted, a bad env value ignored
func delegationKnob[T any](ctx context.Context, key, envVar string, parse func(any) (T, error), def T, invalidMsg string) T {
	if val := LoadDelegationConfig(ctx)[key]; val != nil {
		got, err := parse(val)
		if err != nil {
			delegateToolLog.Warn(strings.Replace(invalidMsg, "%r", pyrepr.Repr(val), 1))
			return def
		}
		return got
	}
	if envVar != "" {
		if envVal := os.Getenv(envVar); envVal != "" {
			if got, err := parse(envVal); err == nil {
				return got
			}
		}
	}
	return def
}

// Python int of a config value as a Go int, saturating values beyond the int range
func delegationInt(v any) (int, error) {
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if n.IsInt64() {
		return int(n.Int64()), nil
	}
	if n.Sign() > 0 {
		return math.MaxInt, nil
	}
	return math.MinInt, nil
}

// Total children one finite one shot session may spawn, zero for unlimited
func OneshotMaxChildren(ctx context.Context) int {
	return delegationKnob(ctx, "oneshot_max_children", "", func(v any) (int, error) {
		n, err := delegationInt(v)
		return max(0, n), err
	}, defaultOneshotMaxChildren, "delegation.oneshot_max_children=%r is not a valid integer; using default 2")
}

// Parallel child limit from config, then DELEGATION_MAX_CONCURRENT_CHILDREN, then ten, floored at one
func MaxConcurrentChildren(ctx context.Context) int {
	result := delegationKnob(ctx, "max_concurrent_children", "DELEGATION_MAX_CONCURRENT_CHILDREN", func(v any) (int, error) {
		n, err := delegationInt(v)
		return max(1, n), err
	}, DefaultMaxConcurrentChildren,
		fmt.Sprintf("delegation.max_concurrent_children=%%r is not a valid integer; using default %d", DefaultMaxConcurrentChildren))
	if result > 10 && LoadDelegationConfig(ctx)["max_concurrent_children"] != nil && highConcurrencyWarned.CompareAndSwap(false, true) {
		delegateToolLog.Warn(fmt.Sprintf("delegation.max_concurrent_children=%d: each child consumes API tokens "+
			"independently. High values multiply cost linearly.", result))
	}
	return result
}

// Whether a background call splits into per task or per group completion messages
func IndependentCompletions(ctx context.Context) bool {
	v, ok := LoadDelegationConfig(ctx)["independent_completions"]
	if !ok {
		v = false
	}
	return isTruthyValue(v, false)
}

var gatewayDelegationModes = map[string]bool{"inline": true, "background": false, "async": false}

// Whether a gateway session joins top level delegations in the turn per delegation.gateway_mode
func GatewayDelegationInline(userConfig any, platformKey string, interimEnabled bool) bool {
	mode := "auto"
	if cfg, ok := userConfig.(map[string]any); ok {
		if section, ok := cfg["delegation"].(map[string]any); ok {
			raw := section["gateway_mode"]
			if raw == nil || !pyrepr.Truthy(raw) {
				raw = "auto"
			}
			mode = pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
		}
	}
	if inline, ok := gatewayDelegationModes[mode]; ok {
		return inline
	}
	return !interimEnabled && !NonMessagingSessionSurfaces[pystr.Lower(platformKey)]
}

type gatewayDelegationInlineKey struct{}

// Context whose agent build carries the joined delegate_task variant
func GatewayDelegationInlineScope(ctx context.Context, inline bool) context.Context {
	return context.WithValue(ctx, gatewayDelegationInlineKey{}, inline)
}

func gatewayDelegationInlineFrom(ctx context.Context) bool {
	inline, _ := ctx.Value(gatewayDelegationInlineKey{}).(bool)
	return inline
}

// Agent state holding the inline decision frozen at its first tool build
type DelegationInlineAgent interface {
	DelegationInline() (inline, set bool)
	SetDelegationInline(inline bool)
}

// Inline decision frozen on the agent at first read from the gateway build scope
func AgentJoinsDelegation(ctx context.Context, a DelegationInlineAgent) bool {
	if cached, set := a.DelegationInline(); set {
		return cached
	}
	cached := gatewayDelegationInlineFrom(ctx)
	a.SetDelegationInline(cached)
	return cached
}

// Whether each child gets its own git worktree off the parent HEAD
func WorktreeIsolation(ctx context.Context) bool {
	return pyrepr.Truthy(LoadDelegationConfig(ctx)["worktree_isolation"])
}

// Background delegation cap equal to the concurrent child limit, a legacy max_async_children key warned once
func MaxAsyncChildren(ctx context.Context) int {
	if LoadDelegationConfig(ctx)["max_async_children"] != nil && legacyMaxAsyncWarned.CompareAndSwap(false, true) {
		delegateToolLog.Warn("delegation.max_async_children is deprecated and ignored; " +
			"delegation.max_concurrent_children now caps background " +
			"delegations too. Remove the stale key from config.yaml.")
	}
	return MaxConcurrentChildren(ctx)
}

// Seconds as nil when non positive, else floored at thirty, an error for a non numeric value
func parseDelegationTimeout(raw any) (*float64, error) {
	parsed, err := pyint.Float(raw)
	if err != nil {
		return nil, err
	}
	if parsed <= 0 {
		return nil, nil
	}
	v := math.Max(delegationChildTimeoutFloorSec, parsed)
	return &v, nil
}

// Inactivity cap for one child in seconds, nil for no cap
func ChildTimeout(ctx context.Context) *float64 {
	return delegationKnob(ctx, "child_timeout_seconds", "DELEGATION_CHILD_TIMEOUT_SECONDS", parseDelegationTimeout, nil,
		"delegation.child_timeout_seconds=%r is not a valid number; using default (no timeout)")
}

// Spawn depth cap floored at one with a warning below the floor
func MaxSpawnDepth(ctx context.Context) int {
	return delegationKnob(ctx, "max_spawn_depth", "", func(v any) (int, error) {
		n, err := delegationInt(v)
		if err != nil {
			return 0, err
		}
		if n < minSpawnDepth {
			delegateToolLog.Warn(fmt.Sprintf("delegation.max_spawn_depth=%d below floor %d; using %d", n, minSpawnDepth, minSpawnDepth))
		}
		return max(minSpawnDepth, n), nil
	}, DelegationMaxDepth, fmt.Sprintf("delegation.max_spawn_depth=%%r is not a valid integer; using default %d", DelegationMaxDepth))
}

// Orchestrator kill switch, true unless a bool false or a falsy string turns it off
func OrchestratorEnabled(ctx context.Context) bool {
	val, ok := LoadDelegationConfig(ctx)["orchestrator_enabled"]
	if !ok {
		return true
	}
	switch x := val.(type) {
	case bool:
		return x
	case string:
		switch pystr.Lower(pystr.Strip(x)) {
		case "true", "1", "yes", "on":
			return true
		}
		return false
	}
	return true
}

// Whether narrowed child toolsets keep the parent MCP toolsets
func InheritMCPToolsets(ctx context.Context) bool {
	return isTruthyValue(LoadDelegationConfig(ctx)["inherit_mcp_toolsets"], true)
}

// DelegationValueError is the ValueError a delegation credential or route resolver refuses a task with
type DelegationValueError struct{ Message string }

func (e *DelegationValueError) Error() string { return e.Message }

// RequirePinnedCommand refuses a pinned ACP transport command that is not on PATH
func RequirePinnedCommand(command, message string) error {
	if command == "" {
		return nil
	}
	if _, err := exec.LookPath(command); err != nil {
		return &DelegationValueError{Message: message}
	}
	return nil
}

// CredentialBundle builds the child credential map every delegation credential branch returns
func CredentialBundle(model, providerName, baseURL, apiKey, apiMode, requestOverrides any, extra map[string]any) map[string]any {
	out := map[string]any{
		"model": model, "provider": providerName, "base_url": baseURL, "api_key": apiKey, "api_mode": apiMode,
		"request_overrides": requestOverrides,
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ResolvedReasoning is a route's final reasoning config used as is, nil standing for the route's own default
type ResolvedReasoning struct{ Config map[string]any }
