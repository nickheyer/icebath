package model

var freeTierRefusalPatterns = []string{"model_not_supported_on_free_tier", "not available on the free tier"}

var billingPatterns = concat([]string{
	"insufficient credits", "insufficient_quota", "insufficient balance", "credit balance",
	"credits exhausted", "credits have been exhausted", "requires available credits",
	"account balance is too low", "no usable credits", "top up your credits", "payment required",
	"billing hard limit", "exceeded your current quota", "account is deactivated", "plan does not include",
	"out of extra usage", "out of funds", "run out of funds", "balance_depleted",
	"budget limit exceeded",
}, freeTierRefusalPatterns, []string{"hard billing limit"})

var unverifiedBillingPatterns = []string{"out of extra usage"}

const xaiSpendingLimitCode = "personal-team-blocked:spending-limit"

const portalUsageWindowCode = "usage_window_and_credits_exhausted"

var billingErrorCodes = setOf(
	"insufficient_quota", "billing_not_active", "payment_required", "insufficient_credits",
	"no_usable_credits", "balance_depleted", "model_not_supported_on_free_tier",
	"member_spend_cap_exceeded", "terminal_quota_exhausted", xaiSpendingLimitCode,
	"credit_balance_exhausted", "organization_spend_limit_exceeded",
	"organization_usage_limit_exceeded", "project_spend_limit_exceeded",
	"insufficient_credits_for_paid_model",
)

var rateLimitPatterns = []string{
	"rate limit", "rate_limit", "too many requests", "throttled", "requests per minute",
	"tokens per minute", "requests per day", "try again in", "please retry after",
	"resource exhausted", "resource_exhausted", "resource-exhausted", "resourceexhausted",
	"rate increased too quickly", "throttlingexception", "too many concurrent requests",
	"servicequotaexceededexception", "throttling",
}

var overloadedPatterns = []string{
	"overloaded", "temporarily overloaded", "service is temporarily overloaded",
	"service may be temporarily overloaded", "server is overloaded", "server overloaded",
	"server overload", "server_overload",
	"service overloaded", "service is overloaded", "upstream overloaded", "currently overloaded",
	"upstream model provider is temporarily unavailable. please try again in a moment.",
	"at capacity", "over capacity",
}

var usageLimitPatterns = []string{"usage limit", "quota", "limit exceeded", "key limit exceeded"}

var usageLimitTransientSignals = []string{
	"try again", "retry", "resets at", "reset in", "resets in", "reset after", "available in",
	"wait", "requests remaining", "periodic", "window", "per minute", "per second",
}

var payloadTooLargePatterns = []string{
	"request entity too large", "payload too large", "error code: 413", "request_too_large",
	"request exceeds the maximum size",
}

var imageTooLargePatterns = []string{
	"image exceeds", "image too large", "image_too_large", "image size exceeds", "image dimensions exceed",
	"dimensions exceed max allowed size", "max allowed size: 8000", "media exceeds", "media too large",
	"patches after processing", "make sure your payload is below", "streamreadconstraints.getmaxstringlength",
}

var imageCorruptPatterns = []string{
	"invalid png image", "invalid jpeg image", "base64 string of provided image cannot be decoded",
	"downloaded response does not contain a valid jpg, png, webp, or ico image",
}

var multimodalToolContentPatterns = []string{
	"text is not set", "tool message content must be a string", "tool content must be a string",
	"tool message must be a string", "expected string, got list", "expected string, got array",
	"tool_call.content must be string", "tool.content.str", "input should be a valid string",
	"chatcompletionrequesttoolmessagecontent",
}

var memoryCeilingPatterns = []string{
	"memory guard", "memory limit exceeded", "memory_guard_tier", "dynamic ceiling",
	"memory ceiling", "available memory", "out of memory", "insufficient memory",
	"prefill would require", "predicted peak would", "prefill safety cap", "metal_cap",
}

var memoryCeilingCodes = setOf("prefill_memory_exceeded", "prefill_memory_aborted", "omlx_prefill_memory_exceeded")

var contextOverflowPatterns = []string{
	"context length", "context size", "maximum context", "token limit", "too many tokens",
	"reduce the length", "exceeds the limit", "context window", "prompt is too long",
	"prompt exceeds max length", "max_tokens", "maximum number of tokens",
	"exceeds the max_model_len", "max_model_len", "prompt length", "input is too long", "maximum model length",
	"context length exceeded", "truncating input",
	"slot context", "n_ctx_slot",
	"超过最大长度", "上下文长度",
	"tokens in request more than max tokens allowed",
	"input is too long", "max input token", "input token", "exceeds the maximum number of input tokens",
	"maximum allowed input length",
}

const CodexEntitlementMarker = "model is not supported when using codex with a chatgpt account"

var modelNotFoundPatterns = []string{
	"is not a valid model", "invalid model", "model not found", "model_not_found", "does not exist",
	"no such model", "unknown model", "unsupported model", "no endpoints found that support tool use",
	"is no longer free",
}

const noUserQuerySignal = "no user query found"

var invalidMessageBodyPatterns = []string{
	"must have non-empty content", "messages must have non-empty", "invalid_request_body",
	"text content blocks must be non-empty", "content field is required",
	"messages: at least one message is required", noUserQuerySignal,
}

var roleAlternationPatterns = []string{
	"roles must alternate", "role must alternate", "must alternate between",
	"consecutive user messages", "consecutive messages with the same role",
	"consecutive messages of the same role", "same role in a row", "multiple user messages in a row",
	"adjacent messages with the same role",
}

var malformedToolArgsPatterns = []string{
	"invalid tool call arguments", "invalid tool_call arguments", "invalid tool_calls arguments",
	"invalid function call arguments", "invalid function_call arguments",
	"tool call arguments are invalid", "tool_call arguments are invalid",
	"function call arguments are invalid", "function_call arguments are invalid",
}

var requestValidationPatterns = []string{
	"unknown parameter", "unsupported parameter", "unrecognized request argument",
	"invalid_request_error", "unknown_parameter", "unsupported_parameter",
}

var validation400Patterns = []string{
	"unknown parameter", "unsupported parameter", "unrecognized request argument",
	"unknown_parameter", "unsupported_parameter",
}

var validation400Codes = setOf("unknown_parameter", "unsupported_parameter")

var validation5xxCodes = setOf("unknown_parameter", "unsupported_parameter", "invalid_request_error")

var serverInjectedParamSenders = []struct {
	param   string
	senders []string
}{
	{"prompt_cache_retention", []string{"meta", "muse", "msl", "model-api", "bedrock", "mantle"}},
}

var paramRejectionWords = []string{"not supported", "unsupported", "unknown", "unrecognized"}

var thinkingMutationWords = []string{"signature", "cannot be modified", "must remain as they were"}

var providerPolicyBlockedPatterns = []string{
	"no endpoints available matching your guardrail", "no endpoints available matching your data policy",
	"no endpoints found matching your data policy",
}

var accountPolicyBlockPatterns = []string{"blocked for a previous policy violation"}

var contentPolicyBlockedPatterns = []string{
	"flagged for possible cybersecurity risk", "trusted access for cyber",
	"violates our usage policies", "violates openai's usage policies", "your request was flagged by",
	"prompt was flagged by our safety", "responses cannot be generated due to safety",
	"content_filter", "responsibleaipolicyviolation", "new_sensitive",
	"content exists risk", "data may contain inappropriate content", "data_inspection_failed",
}

var authPatterns = []string{
	"invalid api key", "invalid_api_key", "gateway_auth_failed", "authentication", "unauthorized",
	"forbidden", "invalid token", "token expired", "token revoked", "access denied",
	"failed to extract accountid from token",
}

var emptyProviderResponsePatterns = []string{
	"returned an empty response", "empty response despite retries", "provider returned an empty response",
	"model returning empty responses", "empty response stream",
}

var timeoutMessagePatterns = []string{
	"timed out", "turn timed out", "request timed out", "deadline exceeded", "operation timed out",
	"upstream timed out",
}

var connectionMessagePatterns = []string{
	"connection refused", "econnrefused", "no route to host", "network is unreachable", "network unreachable",
	"name or service not known", "temporary failure in name resolution", "nodename nor servname provided",
	"getaddrinfo failed", "getaddrinfo enotfound", "eai_again",
	"fetch failed", "failed to fetch",
	"upstream connect error",
}

var serverDisconnectPatterns = []string{
	"server disconnected", "peer closed connection", "connection reset by peer", "connection was closed",
	"network connection lost", "unexpected eof", "incomplete chunked read",
}

var sslCertVerifyPatterns = []string{
	"certificate verify failed", "certificate_verify_failed", "unable to get local issuer certificate",
	"self-signed certificate", "self signed certificate", "certificate has expired",
	"hostname mismatch, certificate is not valid", "unable to verify the first certificate",
}

var sslTransientPatterns = []string{
	"bad record mac", "ssl alert", "tls alert", "ssl handshake failure", "tlsv1 alert", "sslv3 alert",
	"bad_record_mac", "ssl_alert", "tls_alert", "tls_alert_internal_error", "[ssl:",
}

var upstreamBlockedPatterns = []string{
	"your request was blocked", "request blocked", "sorry, you have been blocked",
	"enable javascript and cookies to continue", "cdn-cgi/challenge-platform", "cf-browser-verification",
	"challenge-error-text", "__cf_chl", "cf-error-details", "attention required! | cloudflare",
}

var streamRenderErrorPatterns = []string{"error rendering", "rendering prompt", "jinja template", "jinja render"}

var welcome403NamedPatterns = concat(contentPolicyBlockedPatterns, without(billingPatterns, freeTierRefusalPatterns))

var transient403Codes = setOf("upstream_unavailable")

var status403BillingPatterns = concat([]string{"key limit exceeded", "spending limit"}, billingPatterns)

var quotaWallPatterns = concat([]string{"usage_limit_reached"}, usageLimitPatterns, billingPatterns)

var rateLimitCodes = setOf("resource_exhausted", "throttled", "rate_limit_exceeded")

var modelNotFoundCodes = setOf("model_not_found", "model_not_available", "invalid_model")

var contextOverflowCodes = setOf("context_length_exceeded", "max_tokens_exceeded")

var providerCodeFamilies = map[string]string{
	"openai-codex": "openai", "google": "gemini", "google-gemini": "gemini",
	"google-ai-studio": "gemini", "vertex": "gemini", "google-vertex": "gemini",
}

const codexMaskedReplayMessage = "request blocked."

const codexUnsupportedContentDetail = "unsupported content type"

const imageShrinkTargetBytes = 4 * 1024 * 1024

var resetFields = []string{"resets_in_seconds", "resets_at", "reset_at", "retry_after"}

var resetHeaders = []string{"Retry-After", "X-Ratelimit-Reset"}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func without(list, drop []string) []string {
	var out []string
	for _, s := range list {
		if !containsString(drop, s) {
			out = append(out, s)
		}
	}
	return out
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func setOf(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, s := range items {
		out[s] = true
	}
	return out
}
