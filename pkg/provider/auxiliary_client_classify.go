package provider

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Billing and quota body markers, the main classifier table plus broader phrasings and daily or weekly quota wording
var auxPaymentKeywords = append(append([]string{}, billingPatterns...),
	"credits", "insufficient funds", "can only afford", "billing",
	"isn't available on the free tier", "key limit exceeded", "budget limit",
	"requires a subscription", "upgrade for access", "upgrade for higher limits",
	"reached your session usage limit", "quota exceeded", "quota_exceeded",
	"too many tokens per day", "daily limit", "tokens per day", "daily quota", "resource exhausted",
	"resource_exhausted", "resource-exhausted", "resourceexhausted",
	"weekly usage limit", "weekly limit",
)

// Own status code attribute of an error the way getattr reads it, false where the exception carries none
func auxExcStatusAttr(err error) (int, bool) {
	var sdk *AuxSDKError
	if errors.As(err, &sdk) {
		return sdk.StatusCode, sdk.StatusCode != 0
	}
	var api *APIError
	if errors.As(err, &api) && api.StatusCode != 0 {
		return api.StatusCode, true
	}
	var gem *GeminiAPIError
	if errors.As(err, &gem) && gem.StatusCode != 0 {
		return gem.StatusCode, true
	}
	var cc *ccHTTPError
	if errors.As(err, &cc) && cc.Status != 0 {
		return cc.Status, true
	}
	return 0, false
}

// Python class name of the outermost error, the SDK or transport class before the generic mapping
func auxExcTypeName(err error) string {
	switch e := err.(type) {
	case *AuxSDKError:
		if e.Class != "" {
			return e.Class
		}
		return "APIError"
	case *AuxTransportError:
		if e.Class != "" {
			return e.Class
		}
	}
	return auxPyExceptionName(err)
}

// Reports whether the error is an SDK error of one of the named classes
func auxExcIsSDKClass(err error, classes ...string) bool {
	var sdk *AuxSDKError
	if !errors.As(err, &sdk) {
		return false
	}
	for _, c := range classes {
		if sdk.Class == c {
			return true
		}
	}
	return false
}

// Payment or quota exhaustion from a 402 or a billing body on 403, 404, 429 or no status
func auxIsPaymentError(err error) bool {
	status, ok := auxExcStatusAttr(err)
	if ok && status == 402 {
		return true
	}
	if ok && status != 403 && status != 404 && status != 429 {
		return false
	}
	return auxContainsAny(strings.ToLower(err.Error()), auxPaymentKeywords)
}

// Fresh Nous account lookup seam the paid entitlement check reads
var auxNousPortalAccountInfoFn = GetNousPortalAccountInfo

// True only when a fresh Nous account read reports paid service access allowed
func auxNousPortalAccountHasFreshPaidAccess(ctx context.Context) (paid bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Auxiliary Nous paid-entitlement refresh check failed: " + auxPyStr(r))
			paid = false
		}
	}()
	info := auxNousPortalAccountInfoFn(ctx, true)
	if info == nil {
		slog.Debug("Auxiliary Nous paid-entitlement refresh check failed: 'NoneType' object has no attribute 'paid_service_access'")
		return false
	}
	return info.PaidServiceAccess != nil && *info.PaidServiceAccess
}

var auxRateLimitKeywords = []string{"rate limit", "rate_limit", "too many requests", "try again", "retry after", "resets in"}

var auxRateLimitBillingKeywords = []string{
	"credits", "insufficient funds", "billing", "payment required", "can only afford",
	"out of funds", "run out of funds", "balance_depleted", "no usable credits",
	"model_not_supported_on_free_tier", "not available on the free tier", "isn't available on the free tier",
}

// Upstream capacity wording that backs off without benching the credential
func auxIsOverloadedError(err error) bool {
	return auxContainsAny(strings.ToLower(err.Error()), overloadedPatterns)
}

// A 429 rate limit without billing wording, a RateLimitError class, or a Bedrock throttle
func auxIsRateLimitError(err error) bool {
	if auxExcTypeName(err) == "RateLimitError" {
		return true
	}
	if _, code := auxBotocoreError(err); code == "ThrottlingException" {
		return true
	}
	if auxExcHTTPStatus(err) != 429 {
		return false
	}
	lower := strings.ToLower(err.Error())
	return auxContainsAny(lower, auxRateLimitKeywords) || !auxContainsAny(lower, auxRateLimitBillingKeywords)
}

// Full budget request timeout by SDK class, class name, or timed out wording
func auxIsTimeoutError(err error) bool {
	if auxExcIsSDKClass(err, "APITimeoutError") {
		return true
	}
	return strings.Contains(auxExcTypeName(err), "Timeout") || strings.Contains(strings.ToLower(err.Error()), "timed out")
}

var auxConnectionErrorMarkers = []string{
	"connection refused", "name or service not known", "no route to host",
	"network is unreachable", "timed out", "connection reset",
	"incomplete chunked read", "peer closed connection", "response ended prematurely",
	"unexpected eof", "remoteprotocolerror", "localprotocolerror",
}

// Unreachable endpoint or premature stream close as opposed to a 4xx or 5xx API answer
func auxIsConnectionError(err error) bool {
	if auxExcIsSDKClass(err, "APIConnectionError", "APITimeoutError") {
		return true
	}
	if auxContainsAny(auxExcTypeName(err), []string{"Connection", "Timeout", "DNS", "SSL"}) {
		return true
	}
	return auxContainsAny(strings.ToLower(err.Error()), auxConnectionErrorMarkers)
}

// One off transport blip worth a same provider retry, a connection error or a pure 5xx or 408
func auxIsTransientTransportError(err error) bool {
	if auxIsConnectionError(err) {
		return true
	}
	status := auxExcHTTPStatus(err)
	return status == 408 || (status >= 500 && status < 600)
}

const auxDefaultTransientRetries = 2

// Backoff base in seconds between same provider transient retries, zeroed by tests
var auxTransientRetryBackoffBase = 1.0

// Same provider retries for a transient blip from auxiliary transient_retries clamped to 0 through 6
func auxTransientRetryCount(ctx context.Context) (count int) {
	defer func() {
		if r := recover(); r != nil {
			count = auxDefaultTransientRetries
		}
	}()
	tree, err := auxLoadConfigFn(ctx)
	if err != nil {
		return auxDefaultTransientRetries
	}
	val, _ := config.CfgGet(tree, "auxiliary", "transient_retries")
	if val == nil {
		return auxDefaultTransientRetries
	}
	n, err := auxPyIntCall(val)
	if err != nil {
		return auxDefaultTransientRetries
	}
	return max(0, min(n, 6))
}

// Auth failure that triggers a provider credential refresh, xAI bad credentials 403 included
func auxIsAuthError(err error) bool {
	status, _ := auxExcStatusAttr(err)
	if status == 401 {
		return true
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "error code: 401") || strings.Contains(strings.ToLower(auxExcTypeName(err)), "authenticationerror") {
		return true
	}
	return strings.Contains(lower, "bad-credentials") && (status == 403 || strings.Contains(lower, "unauthenticated"))
}

// Provider rejection naming the parameter together with an unsupported or unknown marker
func auxIsUnsupportedParameterError(err error, param string) bool {
	paramLower := strings.ToLower(param)
	if paramLower == "" {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, paramLower) && auxContainsAny(lower, UnsupportedParamMarkers)
}

// Reports whether a status attribute is absent or one of the 400 and 422 rejection codes
func auxStatusIsRejection(err error) bool {
	status, ok := auxExcStatusAttr(err)
	return !ok || status == 400 || status == 422
}

// 400 or 422 refusing the structured output field on the OpenAI, Anthropic or Gemini wire
func auxIsStructuredOutputRejection(err error) bool {
	if !auxStatusIsRejection(err) {
		return false
	}
	lower := strings.ToLower(err.Error())
	if auxContainsAny(lower, []string{"guided_grammar", "xgrammar", "compile_grammar_error"}) {
		return true
	}
	if strings.Contains(lower, "extra inputs are not permitted") && (strings.Contains(lower, "response_format") || strings.Contains(lower, "output_config")) {
		return true
	}
	if strings.Contains(lower, "response_format") && (strings.Contains(lower, "unavailable") || strings.Contains(lower, "json_schema")) {
		return true
	}
	if auxContainsAny(lower, []string{"response mime type", "response_schema", "response_json_schema"}) {
		return true
	}
	return auxIsUnsupportedParameterError(err, "response_format") || auxIsUnsupportedParameterError(err, "output_config")
}

// 400 or 422 rejecting a reasoning wire control by name
func auxIsReasoningFieldRejection(err error) bool {
	return auxStatusIsRejection(err) && IsReasoningFieldRejection(err.Error())
}

// 400 or 422 refusing to switch reasoning off for an endpoint where it is mandatory
func auxIsReasoningRequiredRejection(err error) bool {
	return auxStatusIsRejection(err) && IsReasoningRequiredRejection(err.Error())
}

// Reasoning wire control keys a profile payload or extra body may carry
var auxProfileReasoningKeys = map[string]bool{
	"reasoning": true, "reasoning_effort": true, "thinking": true, "thinking_config": true, "thinkingconfig": true,
	"thinking_budget": true, "thinkingbudget": true, "enable_thinking": true, "think": true, "verbosity": true,
}

// Copy of the kwargs without any reasoning wire control, nil when nothing was removed
func auxWithoutReasoningFields(kwargs map[string]any) map[string]any {
	retry := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		retry[k] = v
	}
	changed := false
	for _, key := range []string{"reasoning_effort", "_reasoning_config"} {
		if v, ok := retry[key]; ok {
			delete(retry, key)
			changed = changed || v != nil
		}
	}
	if extra, ok := retry["extra_body"].(map[string]any); ok {
		remaining := map[string]any{}
		for k, v := range extra {
			if !auxProfileReasoningKeys[pystr.Lower(pystr.Strip(k))] {
				remaining[k] = v
			}
		}
		if len(remaining) != len(extra) {
			if len(remaining) > 0 {
				retry["extra_body"] = remaining
			} else {
				delete(retry, "extra_body")
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return retry
}

var auxModelNotFoundBillingKeywords = []string{
	"credits", "insufficient funds", "billing", "out of funds", "balance_depleted",
	"no usable credits", "free tier", "free-tier", "not available on the free tier",
}

var auxModelNotFoundMarkers = []string{
	"model does not exist", "does not exist in our configuration", "openrouter catalog",
	"is not a valid model", "no such model", "model not found",
	"the model `", "model_not_found", "unknown model",
}

// Requested model does not exist on a 404, 400 or statusless error without billing wording
func auxIsModelNotFoundError(err error) bool {
	status, ok := auxExcStatusAttr(err)
	lower := strings.ToLower(err.Error())
	if auxContainsAny(lower, auxModelNotFoundBillingKeywords) {
		return false
	}
	if ok && status != 404 && status != 400 {
		return false
	}
	return auxContainsAny(lower, auxModelNotFoundMarkers)
}

var auxModelIncompatibleBillingKeywords = []string{
	"credits", "insufficient funds", "billing", "out of funds", "balance_depleted",
	"no usable credits", "payment required", "free tier", "free-tier",
	"not available on the free tier", "model_not_supported_on_free_tier", "quota",
}

var auxModelIncompatibleMarkers = []string{
	"is not supported when using",
	"model is not supported", "not supported with this", "not supported for this account",
	"model_not_supported", "does not support this model", "unsupported model",
}

// Route that cannot serve the model on a 400 or statusless error, neither not found nor billing
func auxIsModelIncompatibleError(err error) bool {
	status, ok := auxExcStatusAttr(err)
	if ok && status != 400 {
		return false
	}
	lower := strings.ToLower(err.Error())
	if auxIsModelNotFoundError(err) || auxContainsAny(lower, auxModelIncompatibleBillingKeywords) {
		return false
	}
	return auxContainsAny(lower, auxModelIncompatibleMarkers)
}

// Critical path tasks whose full budget timeouts skip the same provider retry
var auxTimeoutNoRetryTasks = map[string]bool{"compression": true, "vision": true, "title_generation": true}

// True when a full budget timeout on a critical path task goes straight to fallback
func auxShouldSkipSameProviderRetry(task string, err error) bool {
	return auxTimeoutNoRetryTasks[task] && auxIsTimeoutError(err) && !strings.Contains(err.Error(), "no-progress timeout")
}

// Drops this profile's cached clients for a provider without closing them so fresh credentials apply
func auxEvictCachedClients(ctx context.Context, provider string) {
	normalized := auxNormalizeAuxProvider(ctx, provider)
	key := home.Key(home.Dir(ctx))
	auxClientCacheMu.Lock()
	defer auxClientCacheMu.Unlock()
	for _, k := range auxClientCacheKeysLocked() {
		if k.Home == key && auxNormalizeAuxProvider(ctx, k.Provider) == normalized {
			auxClientCacheDeleteLocked(k)
		}
	}
}
