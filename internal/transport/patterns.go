package transport

import "github.com/nickheyer/icebath/pkg/strs"

// Words of the refusals the classifier tells apart, as providers word them in lower case
var (
	billing = []string{
		"insufficient credits", "insufficient_quota", "insufficient balance", "credit balance",
		"credits exhausted", "credits have been exhausted", "requires available credits",
		"account balance is too low", "no usable credits", "top up your credits", "payment required",
		"billing hard limit", "exceeded your current quota", "account is deactivated", "plan does not include",
		"out of extra usage", "out of funds", "run out of funds", "balance_depleted",
		"budget limit exceeded", "hard billing limit",
	}
	billingCodes = strs.Set(
		"insufficient_quota", "billing_not_active", "payment_required", "insufficient_credits",
		"no_usable_credits", "balance_depleted", "model_not_supported_on_free_tier",
		"member_spend_cap_exceeded", "terminal_quota_exhausted", xaiSpendingLimit,
		"credit_balance_exhausted", "organization_spend_limit_exceeded",
		"organization_usage_limit_exceeded", "project_spend_limit_exceeded",
		"insufficient_credits_for_paid_model",
	)
	rateLimit = []string{
		"rate limit", "rate_limit", "too many requests", "throttled", "requests per minute",
		"tokens per minute", "requests per day", "try again in", "please retry after",
		"resource exhausted", "resource_exhausted", "resource-exhausted", "resourceexhausted",
		"rate increased too quickly", "throttlingexception", "too many concurrent requests",
		"servicequotaexceededexception", "throttling",
	}
	overloaded = []string{
		"overloaded", "temporarily overloaded", "service is temporarily overloaded",
		"service may be temporarily overloaded", "server is overloaded", "server overloaded",
		"server overload", "server_overload",
		"service overloaded", "service is overloaded", "upstream overloaded", "currently overloaded",
		"upstream model provider is temporarily unavailable. please try again in a moment.",
		"at capacity", "over capacity",
	}
	usageLimit          = []string{"usage limit", "quota", "limit exceeded", "key limit exceeded"}
	usageLimitTransient = []string{
		"try again", "retry", "resets at", "reset in", "resets in", "reset after", "available in",
		"wait", "requests remaining", "periodic", "window", "per minute", "per second",
	}
	payloadTooLarge = []string{
		"request entity too large", "payload too large", "error code: 413", "request_too_large",
		"request exceeds the maximum size",
	}
	imageTooLarge = []string{
		"image exceeds", "image too large", "image_too_large", "image size exceeds", "image dimensions exceed",
		"dimensions exceed max allowed size", "max allowed size: 8000", "media exceeds", "media too large",
		"patches after processing", "make sure your payload is below", "streamreadconstraints.getmaxstringlength",
	}
	imageCorrupt = []string{
		"invalid png image", "invalid jpeg image", "base64 string of provided image cannot be decoded",
		"downloaded response does not contain a valid jpg, png, webp, or ico image",
	}
	multimodalTool = []string{
		"text is not set", "tool message content must be a string", "tool content must be a string",
		"tool message must be a string", "expected string, got list", "expected string, got array",
		"tool_call.content must be string", "tool.content.str", "input should be a valid string",
		"chatcompletionrequesttoolmessagecontent",
	}
	memoryCeiling = []string{
		"memory guard", "memory limit exceeded", "memory_guard_tier", "dynamic ceiling",
		"memory ceiling", "available memory", "out of memory", "insufficient memory",
		"prefill would require", "predicted peak would", "prefill safety cap", "metal_cap",
	}
	memoryCodes     = strs.Set("prefill_memory_exceeded", "prefill_memory_aborted", "omlx_prefill_memory_exceeded")
	contextOverflow = []string{
		"context length", "context size", "maximum context", "token limit", "too many tokens",
		"reduce the length", "exceeds the limit", "context window", "prompt is too long",
		"prompt exceeds max length", "max_tokens", "maximum number of tokens",
		"exceeds the max_model_len", "max_model_len", "prompt length", "input is too long", "maximum model length",
		"context length exceeded", "truncating input", "slot context", "n_ctx_slot",
		"超过最大长度", "上下文长度", "tokens in request more than max tokens allowed",
		"max input token", "input token", "exceeds the maximum number of input tokens", "maximum allowed input length",
	}
	modelNotFound = []string{
		"is not a valid model", "invalid model", "model not found", "model_not_found", "does not exist",
		"no such model", "unknown model", "unsupported model", "no endpoints found that support tool use",
		"is no longer free",
	}
	noUserQuery = "no user query found"
	invalidBody = []string{
		"must have non-empty content", "messages must have non-empty", "invalid_request_body",
		"text content blocks must be non-empty", "content field is required",
		"messages: at least one message is required", noUserQuery,
	}
	roleAlternation = []string{
		"roles must alternate", "role must alternate", "must alternate between",
		"consecutive user messages", "consecutive messages with the same role",
		"consecutive messages of the same role", "same role in a row", "multiple user messages in a row",
		"adjacent messages with the same role",
	}
	malformedArgs = []string{
		"invalid tool call arguments", "invalid tool_call arguments", "invalid tool_calls arguments",
		"invalid function call arguments", "invalid function_call arguments",
		"tool call arguments are invalid", "tool_call arguments are invalid",
		"function call arguments are invalid", "function_call arguments are invalid",
	}
	requestValidation = []string{
		"unknown parameter", "unsupported parameter", "unrecognized request argument",
		"invalid_request_error", "unknown_parameter", "unsupported_parameter",
	}
	validation400 = []string{
		"unknown parameter", "unsupported parameter", "unrecognized request argument", "unknown_parameter", "unsupported_parameter",
	}
	policyBlocked = []string{
		"no endpoints available matching your guardrail", "no endpoints available matching your data policy",
		"no endpoints found matching your data policy",
	}
	accountPolicyBlock   = []string{"blocked for a previous policy violation"}
	contentPolicyBlocked = []string{
		"flagged for possible cybersecurity risk", "trusted access for cyber",
		"violates our usage policies", "violates openai's usage policies", "your request was flagged by",
		"prompt was flagged by our safety", "responses cannot be generated due to safety",
		"content_filter", "responsibleaipolicyviolation", "new_sensitive",
		"content exists risk", "data may contain inappropriate content", "data_inspection_failed",
	}
	auth = []string{
		"invalid api key", "invalid_api_key", "gateway_auth_failed", "authentication", "unauthorized",
		"forbidden", "invalid token", "token expired", "token revoked", "access denied",
		"failed to extract accountid from token",
	}
	emptyResponse = []string{
		"returned an empty response", "empty response despite retries", "provider returned an empty response",
		"model returning empty responses", "empty response stream",
	}
	timeoutMessages = []string{
		"timed out", "turn timed out", "request timed out", "deadline exceeded", "operation timed out", "upstream timed out",
	}
	connectionMessages = []string{
		"connection refused", "econnrefused", "no route to host", "network is unreachable", "network unreachable",
		"name or service not known", "temporary failure in name resolution", "nodename nor servname provided",
		"getaddrinfo failed", "getaddrinfo enotfound", "eai_again", "no such host",
		"fetch failed", "failed to fetch", "upstream connect error",
	}
	disconnects = []string{
		"server disconnected", "peer closed connection", "connection reset by peer", "connection was closed",
		"network connection lost", "unexpected eof", "incomplete chunked read",
	}
	certificateFailures = []string{
		"certificate verify failed", "certificate_verify_failed", "unable to get local issuer certificate",
		"self-signed certificate", "self signed certificate", "certificate has expired",
		"hostname mismatch, certificate is not valid", "unable to verify the first certificate",
		"x509: certificate signed by unknown authority", "x509: certificate has expired",
		"x509: certificate is valid for", "tls: failed to verify certificate",
	}
	tlsTransient = []string{
		"bad record mac", "ssl alert", "tls alert", "ssl handshake failure", "tlsv1 alert", "sslv3 alert",
		"bad_record_mac", "ssl_alert", "tls_alert", "tls_alert_internal_error", "[ssl:",
	}
	renderError      = []string{"error rendering", "rendering prompt", "jinja template", "jinja render"}
	resetFields      = []string{"resets_in_seconds", "resets_at", "reset_at", "retry_after"}
	xaiSpendingLimit = "personal-team-blocked:spending-limit"
)
