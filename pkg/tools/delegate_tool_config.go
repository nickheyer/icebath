package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Lowest spawn depth a delegation runs with whatever is stored
const minSpawnDepth = 1

// Shortest subagent idle limit a set limit is raised to
const delegationChildTimeoutFloor = 30 * time.Second

var highConcurrencyWarned atomic.Bool

// Surfaces with no human chat thread, matching gateway session_context NON_MESSAGING_SESSION_SURFACES
var NonMessagingSessionSurfaces = map[string]bool{
	"": true, "api_server": true, "cli": true, "codex": true, state.ClientHistorySource: true, "gateway": true, "kanban": true,
	"local": true, "msgraph_webhook": true, "tool": true, "tui": true, "webhook": true,
}

// Process wide legacy CLI config, set by the binary, read only under ICEBATH_IGNORE_USER_CONFIG=1
var LoadCLIConfig func(ctx context.Context) map[string]any

// Delegation section of the effective config, the legacy CLI config when user config is ignored, empty on failure
func LoadDelegationConfig(ctx context.Context) map[string]any {
	if os.Getenv("ICEBATH_IGNORE_USER_CONFIG") != "1" {
		h := home.Dir(ctx)
		if tree, err := config.LoadReadonly(home.WithOverride(ctx, h), h, config.NewSecrets(h).Lookup()); err == nil {
			raw := tree["delegation"]
			if raw == nil || !anyutil.IsTrue(raw) {
				return map[string]any{}
			}
			if cfg, ok := raw.(map[string]any); ok {
				return cfg
			}
		}
	}
	if raw := LoadCLIConfig(ctx)["delegation"]; raw != nil && anyutil.IsTrue(raw) {
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
	pkgLog().Warn(fmt.Sprintf("Subagent auto-denied dangerous command: %s (%s). Turn on subagent auto approve in /settings Agent to allow.", command, description))
	return "deny"
}

// One time approval a subagent gets under the subagent auto approve setting, logged for audit
func subagentAutoApprove(command, description string, _ map[string]any) string {
	pkgLog().Warn(fmt.Sprintf("Subagent auto-approved dangerous command: %s (%s)", command, description))
	return "once"
}

// Non interactive approval callback for subagent workers chosen by the subagent auto approve setting
func SubagentApprovalCallback(ctx context.Context) func(command, description string, kwargs map[string]any) string {
	if settings.DelegationSubagentAutoApprove.Get(ctx) {
		return subagentAutoApprove
	}
	return subagentAutoDeny
}

// Total children one finite one shot session may spawn, zero for unlimited
func OneshotMaxChildren(ctx context.Context) int {
	return max(0, settings.DelegationOneshotMaxChildren.Get(ctx))
}

// Parallel child limit, floored at one, warned once when set above ten
func MaxConcurrentChildren(ctx context.Context) int {
	result := max(1, settings.DelegationMaxConcurrentChildren.Get(ctx))
	if v, err := settings.Resolve(ctx, settings.DelegationMaxConcurrentChildren.Key); err == nil && v.Source != settings.SourceDefault &&
		result > 10 && highConcurrencyWarned.CompareAndSwap(false, true) {
		pkgLog().Warn(fmt.Sprintf("%d parallel subagents: each one uses model tokens on its own, so cost grows with the number.", result))
	}
	return result
}

// Whether a background call splits into per task or per group completion messages
func IndependentCompletions(ctx context.Context) bool {
	return settings.DelegationIndependentCompletions.Get(ctx)
}

var gatewayDelegationModes = map[string]bool{"inline": true, "background": false, "async": false}

// Whether a chat session joins top level delegations in the turn, auto deciding from interim messages and the surface
func GatewayDelegationInline(ctx context.Context, platformKey string, interimEnabled bool) bool {
	if inline, ok := gatewayDelegationModes[settings.DelegationGatewayMode.Get(ctx)]; ok {
		return inline
	}
	return !interimEnabled && !NonMessagingSessionSurfaces[strings.ToLower(platformKey)]
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
	return settings.DelegationWorktreeIsolation.Get(ctx)
}

// Background delegation cap, the same as the parallel child limit
func MaxAsyncChildren(ctx context.Context) int {
	return MaxConcurrentChildren(ctx)
}

// Inactivity cap for one child in seconds, nil for no cap, a set cap raised to the floor
func ChildTimeout(ctx context.Context) *float64 {
	d := settings.DelegationChildTimeoutSeconds.Get(ctx)
	if d <= 0 {
		return nil
	}
	v := max(delegationChildTimeoutFloor, d).Seconds()
	return &v
}

// Spawn depth cap floored at one with a warning below the floor
func MaxSpawnDepth(ctx context.Context) int {
	n := settings.DelegationMaxSpawnDepth.Get(ctx)
	if n < minSpawnDepth {
		pkgLog().Warn(fmt.Sprintf("subagent depth %d is below the floor %d; using %d", n, minSpawnDepth, minSpawnDepth))
		return minSpawnDepth
	}
	return n
}

// Orchestrator kill switch
func OrchestratorEnabled(ctx context.Context) bool {
	return settings.DelegationOrchestratorEnabled.Get(ctx)
}

// Whether narrowed child toolsets keep the parent MCP toolsets
func InheritMCPToolsets(ctx context.Context) bool {
	return settings.DelegationInheritMcpToolsets.Get(ctx)
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
