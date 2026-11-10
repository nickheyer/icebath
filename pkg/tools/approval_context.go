package tools

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Longest wait every platform can express, DWORD milliseconds on Windows and one year elsewhere
var MaxSafeTimeoutSeconds = func() float64 {
	if runtime.GOOS == "windows" {
		return float64(0xFFFFFFFF-1000) / 1000.0
	}
	return 31536000.0
}()

// ApprovalHookInvoker dispatches plugin lifecycle hooks such as pre_approval_request, set by the plugin host
var ApprovalHookInvoker func(hookName string, payload map[string]any) error

// ViewerTurnHasApprover reports whether the executing viewer turn has a subscriber holding session approve
var ViewerTurnHasApprover func(env *Env) bool

// FiniteTurnRequired reports the admitted turn's finite flag and whether one is known, the env flag by default
var FiniteTurnRequired func(env *Env) (bool, bool) = FiniteTurnFlag

// RoomApprovalMode reports a hosted room execution policy's approval mode when one governs the turn
var RoomApprovalMode func(env *Env) (string, bool)

// Launch sources of a local session whose viewer answers per command prompts
var localInteractiveSources = setOf("cli", "tui", "gui", "acp")

// Programmatic platforms where no human can answer an approval prompt
var unattendedApprovalPlatforms = setOf("webhook", "msgraph_webhook", "api_server")

// Platforms where a registered notify callback still means nobody can answer
var notifierBlindApprovalPlatforms = setOf("webhook", "msgraph_webhook")

var validApprovalModes = []string{"manual", "smart", "off"}

func storeSessionValue(env *Env, key string, value any) {
	if env != nil && env.Values != nil {
		env.Values.Store(key, value)
	}
}

func loadSessionString(env *Env, key string) (string, bool) {
	if env == nil {
		return "", false
	}
	if v, ok := env.observability.lookup(key); ok {
		return v, true
	}
	if env.Values == nil {
		return "", false
	}
	v, ok := env.Values.Load(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// SetInteractiveContext binds interactive mode for the session instead of mutating the process environment
func SetInteractiveContext(env *Env, interactive bool) {
	storeSessionValue(env, "session.interactive", interactive)
}

// ResetInteractiveContext drops the bound interactive flag so the process environment decides again
func ResetInteractiveContext(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Delete("session.interactive")
	}
}

// Reports an interactive CLI or ACP session, bound flag first and process environment second
func isInteractiveCLI(env *Env) bool {
	return sessionFlag(env, "interactive", "ICEBATH_INTERACTIVE")
}

// Invokes a plugin lifecycle hook with the session correlation ids, never raising
func fireApprovalHook(env *Env, hookName string, payload map[string]any) {
	if ApprovalHookInvoker == nil {
		return
	}
	p := map[string]any{}
	for k, v := range payload {
		p[k] = v
	}
	if _, ok := p["turn_id"]; !ok {
		turn, _ := loadSessionString(env, "approval.turn_id")
		p["turn_id"] = turn
	}
	if _, ok := p["tool_call_id"]; !ok {
		call, _ := loadSessionString(env, "approval.tool_call_id")
		p["tool_call_id"] = call
	}
	if sid, _ := loadSessionString(env, "approval.session_id"); sid != "" {
		if _, ok := p["session_id"]; !ok {
			p["session_id"] = sid
		}
	}
	if err := ApprovalHookInvoker(hookName, p); err != nil {
		envLog(env).Debug("Approval hook dispatch failed", "hook", hookName, "error", err)
	}
}

// Surface a gateway rendered approval reports, cli for a classic CLI session's turn else gateway
func gatewayApprovalSurface(env *Env) string {
	if sessionValue(env, "surface", "ICEBATH_SESSION_SURFACE") == "cli" {
		return "cli"
	}
	return "gateway"
}

// Reports a classic CLI, TUI, Desktop or ACP session's turn from the launch policy the gateway froze
func isLocalInteractiveSession(env *Env) bool {
	return localInteractiveSources[sessionValue(env, "source", "ICEBATH_SESSION_SOURCE")]
}

// Reports a viewer submitted turn whose session has a subscriber able to answer approvals
func isViewerTurnWithApprover(env *Env) bool {
	return ViewerTurnHasApprover != nil && ViewerTurnHasApprover(env)
}

// SetCurrentSessionKey binds the active approval session key to the session
func SetCurrentSessionKey(env *Env, sessionKey string) {
	storeSessionValue(env, "approval.session_key", sessionKey)
}

// ResetCurrentSessionKey drops the bound approval session key
func ResetCurrentSessionKey(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Delete("approval.session_key")
	}
}

// SetCurrentObservabilityContext binds the tool correlation ids forwarded to approval hooks
func SetCurrentObservabilityContext(env *Env, turnID, toolCallID, sessionID string) {
	storeSessionValue(env, "approval.turn_id", turnID)
	storeSessionValue(env, "approval.tool_call_id", toolCallID)
	storeSessionValue(env, "approval.session_id", sessionID)
}

// Correlation ids one tool call binds, the per call contextvars approval hooks and output transforms read
type observabilityIDs struct{ turnID, toolCallID, sessionID string }

func (o *observabilityIDs) lookup(key string) (string, bool) {
	if o == nil {
		return "", false
	}
	switch key {
	case "approval.turn_id":
		return o.turnID, true
	case "approval.tool_call_id":
		return o.toolCallID, true
	case "approval.session_id":
		return o.sessionID, true
	}
	return "", false
}

// WithObservabilityContext copies env with its own tool correlation ids so concurrent calls never read each other's
func WithObservabilityContext(env *Env, turnID, toolCallID, sessionID string) *Env {
	if env == nil {
		return nil
	}
	bound := *env
	bound.observability = &observabilityIDs{turnID: turnID, toolCallID: toolCallID, sessionID: sessionID}
	return &bound
}

// ResetCurrentObservabilityContext drops the bound tool correlation ids
func ResetCurrentObservabilityContext(env *Env) {
	if env != nil && env.Values != nil {
		env.Values.Delete("approval.session_id")
		env.Values.Delete("approval.tool_call_id")
		env.Values.Delete("approval.turn_id")
	}
}

// Active session key, approval binding first, session environment second, default last
func getCurrentSessionKey(env *Env, def string) string {
	if key, _ := loadSessionString(env, "approval.session_key"); key != "" {
		return key
	}
	if v := sessionValue(env, "key", "ICEBATH_SESSION_KEY"); v != "" {
		return v
	}
	return def
}

// Session key of the call's session with the default fallback
func CurrentSessionKey(env *Env) string { return getCurrentSessionKey(env, "default") }

// Approval session key of the call's session with def as fallback, the exported face for pkg/cron and pkg/codeexec
func CurrentSessionKeyOr(env *Env, def string) string { return getCurrentSessionKey(env, def) }

// Current gateway platform from the session values with the process environment as fallback
func getSessionPlatform(env *Env) string {
	return sessionValue(env, "platform", "ICEBATH_SESSION_PLATFORM")
}

// Reports an approval decision running inside cron
func isCronApprovalContext(env *Env) bool {
	return sessionFlag(env, "cron", "ICEBATH_CRON_SESSION")
}

// Reports a programmatic platform where nobody can resolve a pending approval
func isUnattendedPlatformApprovalContext(env *Env) bool {
	return unattendedApprovalPlatforms[getSessionPlatform(env)]
}

// Reports a single query session where nobody waits to answer approvals
func isSingleQueryApprovalContext(env *Env) bool {
	if FiniteTurnRequired != nil {
		if required, known := FiniteTurnRequired(env); known && required {
			return true
		}
	}
	return sessionFlag(env, "single_query", "ICEBATH_SINGLE_QUERY_SESSION")
}

// Reports single query, cron and unattended platform sessions where no user can answer
func NoUserCanAnswer(env *Env) bool {
	return isSingleQueryApprovalContext(env) || isCronApprovalContext(env) || isUnattendedPlatformApprovalContext(env)
}

// Reports a gateway or API session that can answer an approval, never cron or unattended
func IsGatewayApprovalContext(env *Env) bool {
	if isCronApprovalContext(env) || isUnattendedPlatformApprovalContext(env) {
		return false
	}
	return sessionFlag(env, "gateway", "ICEBATH_GATEWAY_SESSION") || getSessionPlatform(env) != ""
}

// Reports a process or subprocess spawned by a delegated child
func isDelegatedChildProcessContext(env *Env) bool {
	if env != nil {
		if env.delegatedChild {
			return true
		}
		if env.Values != nil {
			if v, ok := env.Values.Load("session.delegated_child"); ok {
				if b, _ := v.(bool); b {
					return true
				}
			}
		}
	}
	return os.Getenv(delegatedChildEnvMarker) != ""
}

// Explicit asker, else the session's own human in the loop
func resolveCLIApprovalCallback(env *Env, approvalCallback Asker) Asker {
	if approvalCallback != nil {
		return approvalCallback
	}
	if env != nil && env.Ask != nil {
		return env.Ask
	}
	return nil
}

// Prefers the CLI dangerous command panel over a silent pending approval
func shouldFallThroughToCLIApproval(isCLI, hasCallback, hasNotify bool) bool {
	return isCLI && hasCallback && !hasNotify
}

// Normalizes a configured approval mode, a YAML false reads as off and unknown strings fall back to manual
func normalizeApprovalMode(mode any) string {
	switch m := mode.(type) {
	case bool:
		if !m {
			return "off"
		}
		return "manual"
	case string:
		normalized := strings.ToLower(strings.TrimSpace(m))
		for _, valid := range validApprovalModes {
			if normalized == valid {
				return normalized
			}
		}
		if normalized != "" {
			slog.Default().Warn(fmt.Sprintf("Unknown approvals.mode %q — defaulting to 'manual'. Valid values: %s", m, strings.Join(validApprovalModes, ", ")))
		}
	}
	return "manual"
}

// Live approvals config block, callers must not mutate it
func getApprovalConfig(env *Env) map[string]any {
	section := cfgTree(env).Map("approvals")
	if section == nil {
		return map[string]any{}
	}
	return section
}

// Approval mode manual, smart, or off with a hosted room policy overriding config
func getApprovalMode(env *Env) string {
	if RoomApprovalMode != nil {
		if mode, ok := RoomApprovalMode(env); ok {
			return mode
		}
	}
	mode, ok := getApprovalConfig(env)["mode"]
	if !ok {
		mode = "manual"
	}
	return normalizeApprovalMode(mode)
}

// Exported getApprovalMode for the approvals command of other packages
func GetApprovalMode(env *Env) string { return getApprovalMode(env) }

// Configured approvals.timeout in seconds, default 300, clamped to the platform safe maximum
func getApprovalTimeout(env *Env) int {
	raw := 300
	if v, ok := getApprovalConfig(env)["timeout"]; ok {
		n, isInt := asIntValue(v)
		if !isInt {
			return 300
		}
		raw = n
	}
	safeCap := int(MaxSafeTimeoutSeconds)
	if raw > safeCap {
		slog.Default().Warn(fmt.Sprintf("approvals.timeout=%d exceeds the platform-safe maximum; clamping to %ds", raw, safeCap))
		return safeCap
	}
	return raw
}

// Configured approvals timeout in seconds for the gateway approval prompt
func GetApprovalTimeout(env *Env) int { return getApprovalTimeout(env) }

// One human wording for an approval timeout window shared by every surface
func formatApprovalWindow(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	count, unit := seconds, "second"
	switch {
	case seconds != 0 && seconds%3600 == 0:
		count, unit = seconds/3600, "hour"
	case seconds != 0 && seconds%60 == 0:
		count, unit = seconds/60, "minute"
	}
	if count == 1 {
		return fmt.Sprintf("%d %s", count, unit)
	}
	return fmt.Sprintf("%d %ss", count, unit)
}

// Human wording of an approval timeout window for the gateway approval prompt
func FormatApprovalWindow(seconds int) string { return formatApprovalWindow(seconds) }

// Waited and suggested values for the approval timeout copy
func approvalTimeoutNoticeKwargs(env *Env) map[string]any {
	seconds := getApprovalTimeout(env)
	return map[string]any{"waited": formatApprovalWindow(seconds), "suggested": seconds * 3}
}

// Reads approvals key as approve or deny, default deny
func binaryApprovalMode(env *Env, key string) string {
	raw, ok := getApprovalConfig(env)[key]
	if !ok || raw == nil {
		raw = "deny"
	}
	mode := strings.TrimSpace(strings.ToLower(pyStr(raw)))
	switch mode {
	case "approve", "off", "allow", "yes":
		return "approve"
	}
	return "deny"
}

// Python str() of a config scalar
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case nil:
		return "None"
	}
	return fmt.Sprint(v)
}

func getCronApprovalMode(env *Env) string        { return binaryApprovalMode(env, "cron_mode") }
func getSingleQueryApprovalMode(env *Env) string { return binaryApprovalMode(env, "single_query_mode") }
func getUnattendedApprovalMode(env *Env) string  { return binaryApprovalMode(env, "unattended_mode") }

// Explicitly selected approval transport and the fail closed fallback, empty when none
func getApprovalTransportConfig(env *Env) (string, string) {
	section := config.AsMap(cfgTree(env).Get("security.approval"))
	selected := strings.ToLower(strings.TrimSpace(pyStrOrEmpty(section["transport"])))
	fallback := strings.ToLower(strings.TrimSpace(pyStrOrEmpty(section["transport_fallback"])))
	if selected == "" {
		selected = "builtin"
	}
	if fallback == "builtin" {
		return selected, "builtin"
	}
	return selected, ""
}

// Python str of a value treating None and empty as the empty string
func pyStrOrEmpty(v any) string {
	if v == nil || v == "" {
		return ""
	}
	return pyStr(v)
}
