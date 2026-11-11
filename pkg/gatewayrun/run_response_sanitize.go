package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Programmatic and local surfaces whose replies keep raw text
var gatewayRawTextPlatforms = map[string]bool{"local": true, "api_server": true, "webhook": true, "msgraph_webhook": true}

// Connection failure markers, the first eight also anchoring the provider failure envelope shape
var connectionErrorMarkers = []string{
	`(?:\w+\.)?(?:api\s*)?connection\s*(?:error|timeout)`, `(?:\w+\.)?connect\s*(?:error|timeout)`,
	`connection\s+refused`, `connection\s+reset`, `connection\s+aborted`, `actively\s+refused`,
	`winerror\s+10061\b`, `errno\s+111\b`, `no\s+route\s+to\s+host`, `network\s+is\s+unreachable`,
	`cannot\s+connect`, `failed\s+to\s+establish`, `could\s+not\s+connect`,
}

var connectionInterruptedMarkers = []string{
	`connection\s+reset`, `connection\s+aborted`, `errno\s+104\b`, `errno\s+103\b`,
	`broken\s+pipe`, `server\s+disconnected`, `peer\s+closed\s+connection`,
	`connection\s+was\s+closed`, `network\s+connection\s+lost`, `unexpected\s+eof`,
	`incomplete\s+chunked\s+read`, `response\s+ended\s+prematurely`, `socket\s+hang\s+up`,
	`(?:\w+\.)?remoteprotocolerror`, `(?:\w+\.)?readerror`,
}

var endpointUnreachableMarkers = []string{
	`connection\s+refused`, `actively\s+refused`, `winerror\s+10061\b`, `errno\s+111\b`,
	`no\s+route\s+to\s+host`, `network\s+is\s+unreachable`, `cannot\s+connect`,
	`failed\s+to\s+establish`, `could\s+not\s+connect`, `(?:\w+\.)?connect\s*(?:error|timeout)`,
}

var providerErrorMarkers = []string{
	`api\s+(?:call\s+)?failed`, `provider\s+authentication\s+failed`, `non-retryable\s+error`,
	`rate\s+limited\s+after\s+\d+\s+retries`, `error\s+code\s*:`, `http\s*\d{3}\b`,
	`incorrect\s+api\s+key`, `invalid\s+api\s+key`,
}

// Python regex compiled case insensitive, a bad pattern panicking at init
func mustCompileIgnoreCase(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileIgnoreCase(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

var (
	gatewayProviderPolicyRe = mustCompileIgnoreCase(`(cybersecurity\s+risk|security\s+policy|safety\s+policy|policy\s+violation|violat(?:e|es|ed|ion)` +
		`|blocked\s+(?:because|by|under)|request\s+(?:was\s+)?(?:blocked|rejected)|disallowed|moderation)`)
	gatewayAuthErrorRe = mustCompileIgnoreCase(`(provider\s+authentication\s+failed|incorrect\s+api\s+key|invalid\s+api\s+key` +
		`|(?<![\d:,.])401(?!\d))`)
	gatewayRateLimitRe               = mustCompileIgnoreCase(`(rate\s+limit|rate-limited|\b429\b|quota|usage\s+limit)`)
	gatewayConnectionErrorRe         = mustCompileIgnoreCase("(" + strings.Join(connectionErrorMarkers, "|") + ")")
	gatewayConnectionInterruptedRe   = mustCompileIgnoreCase("(" + strings.Join(connectionInterruptedMarkers, "|") + ")")
	gatewayEndpointUnreachableRe     = mustCompileIgnoreCase("(" + strings.Join(endpointUnreachableMarkers, "|") + ")")
	gatewayProviderErrorShapeRe      = mustCompileIgnoreCase(`^\s*(\W*\s*)?(` + strings.Join(append(append(append([]string{}, providerErrorMarkers...), connectionErrorMarkers[:8]...), `all\s+connection\s+attempts\s+failed`), "|") + ")")
	providerErrorRepliesByPrecedence = []struct {
		re  *regexp2.Regexp
		key string
	}{
		{gatewayRateLimitRe, "gateway.errors.rate_limited"},
		{gatewayAuthErrorRe, "gateway.errors.auth_failed"},
		{gatewayProviderPolicyRe, "gateway.errors.bad_request"},
		{gatewayConnectionInterruptedRe, "gateway.errors.connection_interrupted"},
		{gatewayEndpointUnreachableRe, "gateway.errors.unreachable"},
		{gatewayConnectionErrorRe, "gateway.errors.connection_unknown"},
	}
)

// Whether a Python regex finds a match anywhere in text
func reSearch(re *regexp2.Regexp, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// True only for programmatic or local surfaces that keep raw text
func gatewaySurfacePassesRawText(platform gateway.Platform) bool {
	return gatewayRawTextPlatforms[gatewayPlatformValue(platform)]
}

// Chat reply for a context overflow naming the canonical compress and new commands
func contextOverflowReply(ctx context.Context) string {
	return i18n.T(ctx, "gateway.errors.context_overflow", "", nil)
}

// Rate limit reply naming the reset window a quota refusal carries
func rateLimitReply(ctx context.Context, text string) string {
	delay, _ := provider.ResetDelayFromMessage(text)
	if delay.Seconds() < 120 {
		return i18n.T(ctx, "gateway.errors.rate_limited", "", nil)
	}
	return i18n.T(ctx, "gateway.errors.usage_limit_resets", "", map[string]any{"window": provider.FormatResetWindow(delay)})
}

// Short user safe reply for a raw provider or API error, the first matching category winning
func gatewayProviderErrorReply(ctx context.Context, text string) string {
	for _, row := range providerErrorRepliesByPrecedence {
		if reSearch(row.re, text) {
			if row.re == gatewayRateLimitRe {
				return rateLimitReply(ctx, text)
			}
			return i18n.T(ctx, row.key, "", nil)
		}
	}
	return i18n.T(ctx, "gateway.errors.provider_kept_failing", "", nil)
}

// True for a short provider failure envelope starting with its marker, never for prose citing a status
func looksLikeGatewayProviderError(text string) bool {
	if text == "" {
		return false
	}
	body := pystr.Strip(text)
	if len([]rune(body)) > 400 || strings.Count(body, "\n") > 4 {
		return false
	}
	return reSearch(gatewayProviderErrorShapeRe, body)
}

// Egress scrub of text leaving the gateway for a chat platform, forced even with redaction off
func redactGatewayUserFacingSecrets(ctx context.Context, text string) string {
	return tools.RedactForEgress(&tools.Env{Home: home.Dir(ctx)}, text)
}

// Final reply sanitized for chat surfaces, provider failures as categories and secrets redacted
func sanitizeGatewayFinalResponse(ctx context.Context, platform gateway.Platform, text string) string {
	if text == "" || gatewaySurfacePassesRawText(platform) {
		return text
	}
	text = gateway.TrimTerminalSentinel(state.SanitizeSurrogates(text))
	if strings.HasPrefix(pystr.Strip(text), agent.InterruptWaitingForModelPrefix) {
		return ""
	}
	redacted := redactGatewayUserFacingSecrets(ctx, text)
	if looksLikeGatewayProviderError(redacted) {
		return gatewayProviderErrorReply(ctx, redacted)
	}
	return redacted
}

// Whether display.platforms of the platform explicitly sets the setting
func hasPlatformDisplayOverride(userConfig map[string]any, platformKey, setting string) bool {
	display, ok := userConfig["display"].(map[string]any)
	if !ok {
		return false
	}
	platforms, ok := display["platforms"].(map[string]any)
	if !ok {
		return false
	}
	platformCfg, ok := platforms[platformKey].(map[string]any)
	if !ok {
		return false
	}
	_, present := platformCfg[setting]
	return present
}

// Boolean display setting with an optional platform only opt in for noisy threaded surfaces
func resolveGatewayDisplayBool(ctx context.Context, userConfig map[string]any, platformKey, setting string, def bool, platform gateway.Platform, requirePlatformOverrideFor []gateway.Platform) bool {
	current := gatewayPlatformValue(platform)
	if platform == "" {
		current = gatewayPlatformValue(gateway.Platform(platformKey))
	}
	for _, p := range requirePlatformOverrideFor {
		if gatewayPlatformValue(p) == current && !hasPlatformDisplayOverride(userConfig, platformKey, setting) {
			return false
		}
	}
	switch v := config.ResolveDisplaySetting(ctx, userConfig, platformKey, setting, def).(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(pystr.Strip(v)) {
		case "true", "yes", "1", "on":
			return true
		}
		return false
	case nil:
		return def
	default:
		return pyrepr.Truthy(v)
	}
}

// Scope aware TERMINAL_CWD for footer and context surfaces, an active refusal scope raising
func terminalScopeCwd(ctx context.Context, def string) (string, error) {
	return tools.TerminalEnvRead(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, "TERMINAL_CWD", def)
}

// True for a retry exhausted turn with hidden reasoning and no visible answer beyond the sentinel
func isGatewayHiddenReasoningIncompleteTurn(agentResult map[string]any) bool {
	if agentResult == nil || pyrepr.Truthy(agentResult["failed"]) || pyrepr.Truthy(agentResult["interrupted"]) || !pyrepr.Truthy(agentResult["partial"]) {
		return false
	}
	errorText := ""
	if v := agentResult["error"]; pyrepr.Truthy(v) {
		errorText = pystr.Strip(pyrepr.Str(v))
	}
	if !strings.Contains(strings.ToLower(errorText), "remained incomplete after") {
		return false
	}
	finalResponse := ""
	if v := agentResult["final_response"]; pyrepr.Truthy(v) {
		finalResponse = pystr.Strip(pyrepr.Str(v))
	}
	return finalResponse == "" || finalResponse == errorText
}

// True only when a gateway turn really completed, so the durable resume marker may clear
func shouldClearResumePendingAfterTurn(agentResult map[string]any) bool {
	if agentResult == nil || pyrepr.Truthy(agentResult["interrupted"]) {
		return false
	}
	if pyrepr.Truthy(agentResult["failed"]) || pyrepr.Truthy(agentResult["partial"]) || pyrepr.Truthy(agentResult["error"]) {
		return false
	}
	completed, present := agentResult["completed"]
	return !present || completed != false
}

// Empty or provider envelope replies turned into user facing failure, interruption or no response text
func normalizeEmptyAgentResponse(ctx context.Context, agentResult map[string]any, response string, historyLen int) string {
	isOverflow := isContextOverflowFailureResult(agentResult, historyLen)
	if response != "" && !(isOverflow && looksLikeGatewayProviderError(response)) {
		return response
	}
	t := func(key string, kwargs map[string]any) string { return i18n.T(ctx, key, "", kwargs) }
	if pyrepr.Truthy(agentResult["failed"]) {
		var errorDetail any = agentResult["error"]
		if !pyrepr.Truthy(errorDetail) {
			errorDetail = t("gateway.shared.unknown_error", nil)
		}
		errorStr := strings.ToLower(pyrepr.Str(errorDetail))
		failureReason := ""
		if v := agentResult["failure_reason"]; pyrepr.Truthy(v) {
			failureReason = pyrepr.Str(v)
		}
		if strings.HasPrefix(failureReason, "session_persistence_failed") || strings.Contains(errorStr, "session storage") {
			if strings.HasSuffix(failureReason, ":disk") || strings.Contains(errorStr, "disk") {
				return t("gateway.errors.session_storage_unavailable_disk", nil)
			}
			return t("gateway.errors.session_storage_unavailable", nil)
		}
		if isOverflow {
			return contextOverflowReply(ctx)
		}
		detail := []rune(pyrepr.Str(errorDetail))
		runLog().Warn(fmt.Sprintf("Agent turn failed; reply sanitized for chat. Detail: %s", string(detail[:min(len(detail), 500)])))
		return t("gateway.errors.generic_failed", nil)
	}
	apiCalls := 0
	if v := agentResult["api_calls"]; pyrepr.Truthy(v) {
		if n, err := pyint.Int64(v); err == nil {
			apiCalls = int(n)
		}
	}
	if pyrepr.Truthy(agentResult["interrupted"]) {
		if apiCalls == 0 {
			return t("gateway.errors.interrupted_before_start", nil)
		}
		return response
	}
	if apiCalls > 0 {
		if isGatewayHiddenReasoningIncompleteTurn(agentResult) {
			return ""
		}
		if pyrepr.Truthy(agentResult["partial"]) {
			err := t("gateway.errors.processing_incomplete", nil)
			if v := agentResult["error"]; pyrepr.Truthy(v) {
				err = pyrepr.Str(v)
			}
			failureReason := ""
			if v := agentResult["failure_reason"]; pyrepr.Truthy(v) {
				failureReason = pyrepr.Str(v)
			}
			if agent.SiteFailureCodes[failureReason] && pystr.Strip(err) != "" && !looksLikeGatewayProviderError(err) {
				if strings.HasPrefix(err, "⚠️") {
					return err
				}
				return "⚠️ " + err
			}
			reason := ""
			runes := []rune(err)
			if looksLikeGatewayProviderError(err) {
				runLog().Warn(fmt.Sprintf("Agent turn ended partially; reply sanitized for chat. Detail: %s", string(runes[:min(len(runes), 500)])))
			} else {
				reason = ": " + string(runes[:min(len(runes), 200)])
			}
			return t("gateway.errors.stopped_before_finishing", map[string]any{"reason": reason})
		}
		return t("gateway.errors.no_response", nil)
	}
	if apiCalls == 0 && !pyrepr.Truthy(agentResult["partial"]) {
		return t("gateway.errors.previous_turn_cleanup", nil)
	}
	return response
}
