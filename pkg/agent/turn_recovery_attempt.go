package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Logs one failed attempt with its buffered retry trace and hints, returning type, lowered message, provider, base URL and model
func logAPIErrorAttempt(ctx context.Context, a *AIAgent, s *LoopState, e *apiErrorTurn, elapsed float64, retryable bool) (string, string, string, string, string) {
	errorType := surfaceExceptionName(e.apiError)
	errorMsg := strings.ToLower(e.apiError.Error())
	summary := a.summarizeAPIError(e.apiError)
	attempt := fmt.Sprintf("attempt %d/%d", s.RetryCount, s.MaxRetries)
	if !retryable {
		attempt += ", not retryable"
	}
	pkgLog().Warn(fmt.Sprintf("API call failed (%s) error_type=%s %s summary=%s", attempt, errorType, a.ClientLogContext(), summary))
	providerName, baseURL, model := a.provider, a.baseURL, a.model
	blines(a, fmt.Sprintf("⚠️  %s%s failed: %s", strings.ToUpper(attempt[:1]), attempt[1:], summary))
	if a.verboseLogging {
		statusStr := ""
		if e.statusCode != nil && *e.statusCode != 0 {
			statusStr = fmt.Sprintf(" [HTTP %d]", *e.statusCode)
		}
		blines(a, fmt.Sprintf("   🔌 %s%s  Provider: %s  Model: %s", errorType, statusStr, providerName, model),
			fmt.Sprintf("   🌐 Endpoint: %s", baseURL))
		if e.statusCode != nil && *e.statusCode != 0 && *e.statusCode < 500 {
			var apiErr *provider.APIError
			if errors.As(e.apiError, &apiErr) && apiErr.Body != "" {
				body := []rune(apiErr.Body)
				blines(a, fmt.Sprintf("   📋 Details: %s", string(body[:min(len(body), 300)])))
			}
		}
		blines(a, fmt.Sprintf("   ⏱️  Elapsed: %.2fs  Context: %d msgs, ~%s tokens", elapsed, len(s.APIMessages), groupThousands(s.ApproxTokens)))
	}
	if a.isOpenRouterURL() && strings.Contains(errorMsg, "support tool use") {
		blines(a, fmt.Sprintf("   💡 No OpenRouter providers for %s support tool calling with your current settings.", model))
		if pyTruthy(a.providerPreferencesForAgent(ctx)["only"]) {
			blines(a, "      Your provider_routing.only restriction is filtering out tool-capable providers.",
				"      Try removing the restriction or adding providers that support tools for this model.")
		}
		blines(a, fmt.Sprintf("      Check which providers support tools: https://openrouter.ai/models/%s", model))
	}
	if suggestion := missingVendorPrefixSuggestion(e.apiError, providerName, model); suggestion != "" {
		blines(a, fmt.Sprintf("   💡 Model '%s' is not a valid id for provider %s — it is missing its vendor prefix.", model, providerName),
			fmt.Sprintf("      Did you mean '%s'?  Re-pick it with /model.", suggestion))
	}
	return errorType, errorMsg, providerName, baseURL, model
}

// Prefixed catalogue id when a bare 404 most likely means the vendor prefix was lost
func missingVendorPrefixSuggestion(apiError error, providerName, model string) string {
	if status, ok := surfaceExceptionStatus(apiError); !ok || status != 404 {
		return ""
	}
	return provider.SuggestPrefixedModelID(providerName, model)
}

// Visible text already delivered this turn after the continuation trail collapses, empty when none
func settleDeliveredPartial(ctx context.Context, a *AIAgent, messages *[]*state.Message, currentTurnUserIdx int) string {
	collapsed := a.collapseContinuationTrail(ctx, messages, currentTurnUserIdx, "error", nil)
	if live := a.currentStreamedAssistantText(); strings.TrimSpace(live) != "" {
		if visible := strings.TrimSpace(a.stripThinkBlocks(live)); visible != "" {
			return visible
		}
		return collapsed
	}
	return collapsed
}

// Seconds a provider asked the client to wait, the Retry-After header first, false when absent or zero
func providerRetryAfterSeconds(apiError error) (time.Duration, bool) {
	if d, ok := provider.RetryAfterFromHeaders(apiErrorHeaders(apiError)); ok {
		return d, d > 0
	}
	var sdkErr *provider.AuxSDKError
	if errors.As(apiError, &sdkErr) {
		return provider.BodyRetryAfter(sdkErr.Body)
	}
	var apiErr *provider.APIError
	if !errors.As(apiError, &apiErr) {
		return 0, false
	}
	return provider.ProviderRetryAfter(apiErr)
}

// fast fail on 100% usage...
func rateLimitOutlastsRetries(resetAt any, now time.Time) bool {
	if resetAt == nil {
		return false
	}
	epoch, ok := provider.ParseAbsoluteTimestamp(resetAt)
	if !ok {
		return false
	}
	return epoch-float64(now.UnixNano())/1e9 > (2 * provider.RetryAfterCap).Seconds()
}

// Picks and announces the wait before the next retry, retry after first, adaptive for rate limits and Z.AI overloads
func computeErrorBackoff(a *AIAgent, apiError error, retryCount, maxRetries int, isRateLimited, isZAICodingOverload bool, baseURL, model string) float64 {
	retryAfter, hasRetryAfter := providerRetryAfterSeconds(apiError)
	if hasRetryAfter {
		retryAfter = min(retryAfter, provider.RetryAfterCap)
	}
	wait := retryAfter
	if !hasRetryAfter {
		wait = provider.JitteredBackoff(retryCount, provider.BackoffOptions{BaseDelay: 2 * time.Second, MaxDelay: 60 * time.Second, JitterRatio: 0.5})
	}
	policy := ""
	adaptive := isRateLimited || isZAICodingOverload
	if adaptive && !hasRetryAfter {
		wait, policy = provider.AdaptiveRateLimitBackoff(retryCount, baseURL, model, apiError, wait)
	}
	reset := ""
	if adaptive {
		reset = provider.ResetHint(apiError, time.Now())
	}
	waitReason := "Rate limited"
	if isZAICodingOverload && !isRateLimited {
		waitReason = "Provider overloaded"
	}
	var status string
	announceNow := false
	if adaptive {
		resetText := ""
		if reset != "" {
			resetText = " Resets in " + reset + "."
		}
		status = fmt.Sprintf("⏱️ %s.%s Waiting %.1fs (attempt %d/%d)%s...", waitReason, resetText, wait.Seconds(), retryCount+1, maxRetries, provider.ZAIPolicyNote(policy))
		announceNow = policy == "zai_coding_overload_long"
	} else {
		status = fmt.Sprintf("⏳ Retrying in %.1fs (attempt %d/%d)...", wait.Seconds(), retryCount, maxRetries)
		announceNow = hasRetryAfter && retryAfter > provider.LiveRetryWaitCap
	}
	if announceNow {
		a.emitDiagnosticStatus(status)
	} else {
		a.BufferDiagnosticStatus(status)
	}
	liveReason := "waiting on provider —"
	if reset != "" {
		liveReason = fmt.Sprintf("%s — resets in %s,", strings.ToLower(waitReason), reset)
	}
	a.emitDiagnosticWait(fmt.Sprintf("⏳ %s retrying in %.0fs (attempt %d/%d)", liveReason, wait.Seconds(), retryCount, maxRetries))
	if policy == "" {
		policy = "default"
	}
	pkgLog().Warn(fmt.Sprintf("Retrying API call in %ss (attempt %d/%d) %s policy=%s error=%s",
		pyrepr.Float(wait.Seconds()), retryCount, maxRetries, a.ClientLogContext(), policy, apiError))
	return wait.Seconds()
}

// Records a Codex ChatGPT account 400 rejecting the model once no pool entry remains eligible for it
func markEntitlementRejectedModel(a *AIAgent, apiError error) bool {
	if status, ok := surfaceExceptionStatus(apiError); !ok || status != 400 {
		return false
	}
	message := apiError.Error()
	var apiErr *provider.APIError
	if errors.As(apiError, &apiErr) && apiErr.Message != "" {
		message = apiErr.Message
	}
	if !strings.Contains(strings.ToLower(message), provider.CodexAccountModelEntitlementMarker) {
		return false
	}
	providerName := strings.ToLower(strings.TrimSpace(a.provider))
	model := strings.TrimSpace(a.model)
	if providerName == "" || model == "" {
		return false
	}
	if a.credentialPool != nil && a.credentialPool.HasAvailable(model) {
		return false
	}
	if a.entitlementRejectedModels == nil {
		a.entitlementRejectedModels = map[[2]string]bool{}
	}
	key := [2]string{providerName, model}
	if a.entitlementRejectedModels[key] {
		return true
	}
	a.entitlementRejectedModels[key] = true
	pkgLog().Warn(fmt.Sprintf("Model entitlement rejection: this account is not entitled to %s via %s; treating it as unavailable for this session", model, providerName))
	a.BufferDiagnosticStatus(fmt.Sprintf("🚫 This account is not entitled to %s via %s; it will be skipped until restart. Switch to an entitled model via /model or `icebath model`.", model, providerName))
	return true
}
