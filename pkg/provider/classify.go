package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Synthetic code for a stream data field rejected before any completion chunk arrived
const ProviderStreamNonJSONErrorCode = "provider_stream_non_json_data"

// Same rejection with an empty payload so the session switches to non streaming
const ProviderStreamEmptyFrameErrorCode = "provider_stream_empty_frame"

// Codex ChatGPT account entitlement wording for a slug the account can never use
const CodexAccountModelEntitlementMarker = "model is not supported when using codex with a chatgpt account"

// EmptyStreamError is raised when a provider closes a stream without yielding a response
type EmptyStreamError struct{ Message string }

func (e *EmptyStreamError) Error() string {
	if e.Message == "" {
		return "provider closed the stream without a response"
	}
	return e.Message
}

// MoAPresetNotFoundError is raised when a persisted MoA preset no longer exists in config
type MoAPresetNotFoundError struct{ Preset string }

func (e *MoAPresetNotFoundError) Error() string { return "MoA preset not found: " + e.Preset }

// FailoverReason names why an API call failed and so which recovery runs
type FailoverReason string

const (
	ReasonAuth                       FailoverReason = "auth"
	ReasonAuthPermanent              FailoverReason = "auth_permanent"
	ReasonBilling                    FailoverReason = "billing"
	ReasonRateLimit                  FailoverReason = "rate_limit"
	ReasonUpstreamRateLimit          FailoverReason = "upstream_rate_limit"
	ReasonUpstreamBlocked            FailoverReason = "upstream_blocked"
	ReasonOverloaded                 FailoverReason = "overloaded"
	ReasonServerError                FailoverReason = "server_error"
	ReasonTimeout                    FailoverReason = "timeout"
	ReasonSSLCertVerification        FailoverReason = "ssl_cert_verification"
	ReasonContextOverflow            FailoverReason = "context_overflow"
	ReasonPayloadTooLarge            FailoverReason = "payload_too_large"
	ReasonImageTooLarge              FailoverReason = "image_too_large"
	ReasonImageCorrupt               FailoverReason = "image_corrupt"
	ReasonModelNotFound              FailoverReason = "model_not_found"
	ReasonProviderPolicyBlocked      FailoverReason = "provider_policy_blocked"
	ReasonDataPolicyDenied           FailoverReason = "data_policy_denied"
	ReasonContentPolicyBlocked       FailoverReason = "content_policy_blocked"
	ReasonModelEntitlement           FailoverReason = "model_entitlement"
	ReasonIncompleteResponse         FailoverReason = "incomplete_response"
	ReasonFormatError                FailoverReason = "format_error"
	ReasonRoleAlternation            FailoverReason = "role_alternation"
	ReasonInvalidEncryptedContent    FailoverReason = "invalid_encrypted_content"
	ReasonMultimodalToolContent      FailoverReason = "multimodal_tool_content_unsupported"
	ReasonReasoningMandatory         FailoverReason = "reasoning_mandatory"
	ReasonThinkingSignature          FailoverReason = "thinking_signature"
	ReasonLongContextTier            FailoverReason = "long_context_tier"
	ReasonOAuthLongContextBetaForbid FailoverReason = "oauth_long_context_beta_forbidden"
	ReasonLlamaCppGrammarPattern     FailoverReason = "llama_cpp_grammar_pattern"
	ReasonCancelled                  FailoverReason = "cancelled"
	ReasonUnknown                    FailoverReason = "unknown"
)

// Classified is the structured verdict on one failed call with its recovery hints
type Classified struct {
	Reason                 FailoverReason
	StatusCode             int
	Provider               string
	Model                  string
	Message                string
	Context                map[string]any
	Retryable              bool
	ShouldCompress         bool
	ShouldRotateCredential bool
	ShouldFallback         bool
}

// IsAuth reports a transient or permanent auth verdict
func (c *Classified) IsAuth() bool {
	return c.Reason == ReasonAuth || c.Reason == ReasonAuthPermanent
}

// BillingUnverified reports a billing verdict resting on an ambiguous body
func (c *Classified) BillingUnverified() bool {
	v, _ := c.Context["billing_unverified"].(bool)
	return v
}

// ResetAt returns the provider declared reopening instant carried in the context
func (c *Classified) ResetAt() (time.Time, bool) {
	switch v := c.Context["reset_at"].(type) {
	case time.Time:
		return v, true
	case float64:
		return time.Unix(0, int64(v*float64(time.Second))), true
	}
	return time.Time{}, false
}

// Reasons the retry loop keeps retrying with backoff before any fallback cascade
var RetryableClientReasons = map[FailoverReason]bool{
	ReasonRateLimit: true, ReasonUpstreamRateLimit: true, ReasonOverloaded: true,
	ReasonContextOverflow: true, ReasonPayloadTooLarge: true, ReasonLongContextTier: true,
	ReasonThinkingSignature: true,
}

var (
	freeTierRefusalPatterns = []string{"model_not_supported_on_free_tier", "not available on the free tier"}
	billingPatterns         = append([]string{
		"insufficient credits", "insufficient_quota", "insufficient balance", "credit balance",
		"credits exhausted", "credits have been exhausted", "requires available credits",
		"account balance is too low", "no usable credits", "top up your credits", "payment required",
		"billing hard limit", "exceeded your current quota", "account is deactivated", "plan does not include",
		"out of extra usage", "out of funds", "run out of funds", "balance_depleted",
		"budget limit exceeded",
	}, append(freeTierRefusalPatterns, "hard billing limit")...)
	unverifiedBillingPatterns = []string{"out of extra usage"}
	xaiSpendingLimitErrorCode = "personal-team-blocked:spending-limit"
	nousUsageWindow402Code    = "usage_window_and_credits_exhausted"
	billingErrorCodes         = setOf(
		"insufficient_quota", "billing_not_active", "payment_required", "insufficient_credits",
		"no_usable_credits", "balance_depleted", "model_not_supported_on_free_tier",
		"member_spend_cap_exceeded", "terminal_quota_exhausted", xaiSpendingLimitErrorCode,
		"credit_balance_exhausted", "organization_spend_limit_exceeded",
		"organization_usage_limit_exceeded", "project_spend_limit_exceeded",
		"insufficient_credits_for_paid_model",
	)
	rateLimitPatterns = []string{
		"rate limit", "rate_limit", "too many requests", "throttled", "requests per minute",
		"tokens per minute", "requests per day", "try again in", "please retry after",
		"resource exhausted", "resource_exhausted", "resource-exhausted", "resourceexhausted",
		"rate increased too quickly", "throttlingexception", "too many concurrent requests",
		"servicequotaexceededexception", "throttling",
	}
	overloadedPatterns = []string{
		"overloaded", "temporarily overloaded", "service is temporarily overloaded",
		"service may be temporarily overloaded", "server is overloaded", "server overloaded",
		"server overload", "server_overload",
		"service overloaded", "service is overloaded", "upstream overloaded", "currently overloaded",
		"upstream model provider is temporarily unavailable. please try again in a moment.",
		"at capacity", "over capacity",
	}
	usageLimitPatterns         = []string{"usage limit", "quota", "limit exceeded", "key limit exceeded"}
	usageLimitTransientSignals = []string{
		"try again", "retry", "resets at", "reset in", "resets in", "reset after", "available in",
		"wait", "requests remaining", "periodic", "window", "per minute", "per second",
	}
	payloadTooLargePatterns = []string{
		"request entity too large", "payload too large", "error code: 413", "request_too_large",
		"request exceeds the maximum size",
	}
	imageTooLargePatterns = []string{
		"image exceeds", "image too large", "image_too_large", "image size exceeds", "image dimensions exceed",
		"dimensions exceed max allowed size", "max allowed size: 8000", "media exceeds", "media too large",
		"patches after processing", "make sure your payload is below", "streamreadconstraints.getmaxstringlength",
	}
	imageCorruptPatterns = []string{
		"invalid png image", "invalid jpeg image", "base64 string of provided image cannot be decoded",
		"downloaded response does not contain a valid jpg, png, webp, or ico image",
	}
	multimodalToolContentPatterns = []string{
		"text is not set", "tool message content must be a string", "tool content must be a string",
		"tool message must be a string", "expected string, got list", "expected string, got array",
		"tool_call.content must be string", "tool.content.str", "input should be a valid string",
		"chatcompletionrequesttoolmessagecontent",
	}
	memoryCeilingPatterns = []string{
		"memory guard", "memory limit exceeded", "memory_guard_tier", "dynamic ceiling",
		"memory ceiling", "available memory", "out of memory", "insufficient memory",
		"prefill would require", "predicted peak would", "prefill safety cap", "metal_cap",
	}
	memoryCeilingErrorCodes = setOf("prefill_memory_exceeded", "prefill_memory_aborted", "omlx_prefill_memory_exceeded")
	contextOverflowPatterns = []string{
		"context length", "context size", "maximum context", "token limit", "too many tokens",
		"reduce the length", "exceeds the limit", "context window", "prompt is too long",
		"prompt exceeds max length", "max_tokens", "maximum number of tokens",
		"exceeds the max_model_len", "max_model_len", "prompt length", "input is too long", "maximum model length",
		"context length exceeded", "truncating input",
		"slot context", "n_ctx_slot",
		"\u8d85\u8fc7\u6700\u5927\u957f\u5ea6", "\u4e0a\u4e0b\u6587\u957f\u5ea6",
		"tokens in request more than max tokens allowed",
		"input is too long", "max input token", "input token", "exceeds the maximum number of input tokens",
		"maximum allowed input length",
	}
	modelNotFoundPatterns = []string{
		"is not a valid model", "invalid model", "model not found", "model_not_found", "does not exist",
		"no such model", "unknown model", "unsupported model", "no endpoints found that support tool use",
		"is no longer free",
	}
	noUserQuerySignal          = "no user query found"
	invalidMessageBodyPatterns = []string{
		"must have non-empty content", "messages must have non-empty", "invalid_request_body",
		"text content blocks must be non-empty", "content field is required",
		"messages: at least one message is required", noUserQuerySignal,
	}
	roleAlternationPatterns = []string{
		"roles must alternate", "role must alternate", "must alternate between",
		"consecutive user messages", "consecutive messages with the same role",
		"consecutive messages of the same role", "same role in a row", "multiple user messages in a row",
		"adjacent messages with the same role",
	}
	malformedToolArgsPatterns = []string{
		"invalid tool call arguments", "invalid tool_call arguments", "invalid tool_calls arguments",
		"invalid function call arguments", "invalid function_call arguments",
		"tool call arguments are invalid", "tool_call arguments are invalid",
		"function call arguments are invalid", "function_call arguments are invalid",
	}
	requestValidationPatterns = []string{
		"unknown parameter", "unsupported parameter", "unrecognized request argument",
		"invalid_request_error", "unknown_parameter", "unsupported_parameter",
	}
	serverInjectedParamSenders = map[string][]string{
		"prompt_cache_retention": {"meta", "muse", "msl", "model-api", "bedrock", "mantle"},
	}
	paramRejectionWords           = []string{"not supported", "unsupported", "unknown", "unrecognized"}
	thinkingMutationWords         = []string{"signature", "cannot be modified", "must remain as they were"}
	providerPolicyBlockedPatterns = []string{
		"no endpoints available matching your guardrail", "no endpoints available matching your data policy",
		"no endpoints found matching your data policy",
	}
	accountPolicyBlockPatterns   = []string{"blocked for a previous policy violation"}
	contentPolicyBlockedPatterns = []string{
		"flagged for possible cybersecurity risk", "trusted access for cyber",
		"violates our usage policies", "violates openai's usage policies", "your request was flagged by",
		"prompt was flagged by our safety", "responses cannot be generated due to safety",
		"content_filter", "responsibleaipolicyviolation", "new_sensitive",
		"content exists risk", "data may contain inappropriate content", "data_inspection_failed",
	}
	authPatterns = []string{
		"invalid api key", "invalid_api_key", "gateway_auth_failed", "authentication", "unauthorized",
		"forbidden", "invalid token", "token expired", "token revoked", "access denied",
		"failed to extract accountid from token",
	}
	emptyProviderResponsePatterns = []string{
		"returned an empty response", "empty response despite retries", "provider returned an empty response",
		"model returning empty responses", "empty response stream",
	}
	timeoutMessagePatterns = []string{
		"timed out", "turn timed out", "request timed out", "deadline exceeded", "operation timed out",
		"upstream timed out",
	}
	connectionMessagePatterns = []string{
		"connection refused", "econnrefused", "no route to host", "network is unreachable", "network unreachable",
		"name or service not known", "temporary failure in name resolution", "nodename nor servname provided",
		"getaddrinfo failed", "getaddrinfo enotfound", "eai_again",
		"fetch failed", "failed to fetch",
		"upstream connect error",
	}
	serverDisconnectPatterns = []string{
		"server disconnected", "peer closed connection", "connection reset by peer", "connection was closed",
		"network connection lost", "unexpected eof", "incomplete chunked read",
	}
	sslCertVerifyPatterns = []string{
		"certificate verify failed", "certificate_verify_failed", "unable to get local issuer certificate",
		"self-signed certificate", "self signed certificate", "certificate has expired",
		"hostname mismatch, certificate is not valid", "unable to verify the first certificate",
		"x509: certificate signed by unknown authority", "x509: certificate has expired",
		"x509: certificate is valid for", "tls: failed to verify certificate",
	}
	sslTransientPatterns = []string{
		"bad record mac", "ssl alert", "tls alert", "ssl handshake failure", "tlsv1 alert", "sslv3 alert",
		"bad_record_mac", "ssl_alert", "tls_alert", "tls_alert_internal_error", "[ssl:",
	}
	upstreamBlockedPatterns = []string{
		"your request was blocked", "request blocked", "sorry, you have been blocked",
		"enable javascript and cookies to continue", "cdn-cgi/challenge-platform", "cf-browser-verification",
		"challenge-error-text", "__cf_chl", "cf-error-details", "attention required! | cloudflare",
	}
	streamRenderErrorPatterns = []string{"error rendering", "rendering prompt", "jinja template", "jinja render"}
	reasoningMandatoryPattern = "reasoning is mandatory"
	welcome403NamedPatterns   = append(append([]string{}, contentPolicyBlockedPatterns...), without(billingPatterns, freeTierRefusalPatterns)...)
	validation400Codes        = setOf("unknown_parameter", "unsupported_parameter")
	validation5xxCodes        = setOf("unknown_parameter", "unsupported_parameter", "invalid_request_error")
	validation400Patterns     = without(requestValidationPatterns, []string{"invalid_request_error"})
	transient403Codes         = setOf("upstream_unavailable")
	resetFields               = []string{"resets_in_seconds", "resets_at", "reset_at", "retry_after"}
	resetHeaders              = []string{"retry-after", "x-ratelimit-reset"}
	providerCodeFamilies      = map[string]string{"openai-codex": "openai", "google": "gemini", "google-gemini": "gemini",
		"google-ai-studio": "gemini", "vertex": "gemini", "google-vertex": "gemini"}
)

// Generic markers a provider 400 puts next to the offending parameter name
var UnsupportedParamMarkers = []string{
	"unsupported parameter", "unsupported_parameter", "not supported", "does not support",
	"doesn't support", "is deprecated for this model",
	"unknown parameter", "unrecognized request argument", "unrecognized parameter",
	"invalid parameter", "extra inputs are not permitted",
	"invalid option: expected one of",
}

var (
	reasoningFieldTokens      = []string{"reasoning_effort", "thinking_config", "thinking_budget", "enable_thinking", "thinkingconfig", "thinkingbudget", "reasoning", "thinking", "think"}
	reasoningParamRejectionRe = regexp.MustCompile(`['"]param['"]\s*:\s*['"](?:reasoning(?:[._]effort)?|thinking(?:_config|_budget)?|enable_thinking)['"]|invalid_reasoning_effort`)
	reasoningModelsTailRe     = regexp.MustCompile(`^\s+models?\b`)
	reasoningRequiredMarkers  = []string{"mandatory", "cannot be disabled", "can't be disabled", "must be enabled", "is required", "always enabled", "cannot be turned off"}
	reasoningLevelMarkers     = []string{"must be one of", "invalid value for"}
)

func setOf(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, s := range items {
		m[s] = true
	}
	return m
}

func without(items, drop []string) []string {
	skip := setOf(drop...)
	out := make([]string, 0, len(items))
	for _, s := range items {
		if !skip[s] {
			out = append(out, s)
		}
	}
	return out
}

func anyIn(msg string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func isTokenChar(b byte) bool {
	return b == '_' || b == '-' || b == '/' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// Leftmost standalone reasoning field token excluding a model id segment or the adjective before models
func reasoningFieldToken(msg string) (int, int, bool) {
	for i := 0; i < len(msg); i++ {
		if i > 0 && isTokenChar(msg[i-1]) {
			continue
		}
		for _, kw := range reasoningFieldTokens {
			if !strings.HasPrefix(msg[i:], kw) {
				continue
			}
			end := i + len(kw)
			if end < len(msg) && isTokenChar(msg[end]) {
				continue
			}
			if reasoningModelsTailRe.MatchString(msg[end:]) {
				continue
			}
			return i, end, true
		}
	}
	return 0, 0, false
}

func window(msg string, start, end, before, after int) string {
	lo := start - before
	if lo < 0 {
		lo = 0
	}
	hi := end + after
	if hi > len(msg) {
		hi = len(msg)
	}
	return msg[lo:hi]
}

// IsReasoningLevelRejection matches a 400 refusing the disabled level against a closed effort set
func IsReasoningLevelRejection(errorMsg string) bool {
	msg := strings.ToLower(errorMsg)
	s, e, ok := reasoningFieldToken(msg)
	if !ok {
		return false
	}
	return anyIn(window(msg, s, e, 48, 96), reasoningLevelMarkers)
}

// IsReasoningRequiredRejection matches a 400 saying reasoning cannot be switched off
func IsReasoningRequiredRejection(errorMsg string) bool {
	msg := strings.ToLower(errorMsg)
	s, e, ok := reasoningFieldToken(msg)
	if !ok {
		return false
	}
	return anyIn(window(msg, s, e, 48, 96), reasoningRequiredMarkers) || IsReasoningLevelRejection(msg)
}

// IsReasoningFieldRejection matches a 400 rejecting a reasoning wire control by name
func IsReasoningFieldRejection(errorMsg string) bool {
	msg := strings.ToLower(errorMsg)
	if reasoningParamRejectionRe.MatchString(msg) {
		return true
	}
	s, e, ok := reasoningFieldToken(msg)
	if !ok {
		return false
	}
	return strings.Contains(window(msg, s, e, 32, 32), "unsupported") || anyIn(msg, UnsupportedParamMarkers)
}

// Verdict is the reason plus hint overrides a stage decided on
type Verdict struct {
	Reason                 FailoverReason
	Retryable              *bool
	ShouldCompress         *bool
	ShouldRotateCredential *bool
	ShouldFallback         *bool
	Context                map[string]any
}

func bptr(b bool) *bool { return &b }

func v(reason FailoverReason) *Verdict { return &Verdict{Reason: reason} }

func (vd *Verdict) rotateFallback() *Verdict {
	vd.ShouldRotateCredential, vd.ShouldFallback = bptr(true), bptr(true)
	return vd
}

func (vd *Verdict) abortFallback() *Verdict {
	vd.Retryable, vd.ShouldFallback = bptr(false), bptr(true)
	return vd
}

func (vd *Verdict) retryable(b bool) *Verdict { vd.Retryable = bptr(b); return vd }
func (vd *Verdict) compress(b bool) *Verdict  { vd.ShouldCompress = bptr(b); return vd }
func (vd *Verdict) fallback(b bool) *Verdict  { vd.ShouldFallback = bptr(b); return vd }
func (vd *Verdict) rotate(b bool) *Verdict    { vd.ShouldRotateCredential = bptr(b); return vd }
func (vd *Verdict) ctx(c map[string]any) *Verdict {
	vd.Context = c
	return vd
}

func vBilling() *Verdict          { return v(ReasonBilling).retryable(false).rotateFallback() }
func vRateLimit() *Verdict        { return v(ReasonRateLimit).rotateFallback() }
func vAuthRotate() *Verdict       { return v(ReasonAuth).retryable(false).rotateFallback() }
func vAuthFallback() *Verdict     { return v(ReasonAuth).abortFallback() }
func vModelNotFound() *Verdict    { return v(ReasonModelNotFound).abortFallback() }
func vUpstreamBlocked() *Verdict  { return v(ReasonUpstreamBlocked).abortFallback() }
func vContentBlocked() *Verdict   { return v(ReasonContentPolicyBlocked).abortFallback() }
func vModelEntitlement() *Verdict { return v(ReasonModelEntitlement).retryable(false).rotateFallback() }
func vFormatError() *Verdict      { return v(ReasonFormatError).abortFallback() }
func vPolicyBlocked() *Verdict    { return v(ReasonProviderPolicyBlocked).abortFallback() }
func vSSLCert() *Verdict          { return v(ReasonSSLCertVerification).abortFallback() }
func vContextOverflow() *Verdict  { return v(ReasonContextOverflow).compress(true) }
func vPayloadTooLarge() *Verdict  { return v(ReasonPayloadTooLarge).compress(true) }
func vOverloaded() *Verdict       { return v(ReasonOverloaded) }
func vServerError() *Verdict      { return v(ReasonServerError) }
func vTimeout() *Verdict          { return v(ReasonTimeout) }
func vUnknown() *Verdict          { return v(ReasonUnknown) }
func vImageTooLarge() *Verdict    { return v(ReasonImageTooLarge) }
func vImageCorrupt() *Verdict     { return v(ReasonImageCorrupt) }
func vMultimodal() *Verdict       { return v(ReasonMultimodalToolContent) }
func vInvalidEncrypted() *Verdict { return v(ReasonInvalidEncryptedContent) }
func vReasoningMandatory() *Verdict {
	return v(ReasonReasoningMandatory).compress(false).fallback(false)
}
func vRoleAlternation() *Verdict   { return v(ReasonRoleAlternation).abortFallback() }
func vMalformedToolArgs() *Verdict { return v(ReasonFormatError).retryable(false).fallback(false) }

func billingHints(msg string) *Verdict {
	vd := vBilling()
	if anyIn(msg, unverifiedBillingPatterns) {
		vd.Context = map[string]any{"billing_unverified": true, "possible_content_filter": true}
	}
	return vd
}

type rule struct {
	patterns []string
	verdict  func(msg string) *Verdict
}

func fixed(f func() *Verdict) func(string) *Verdict { return func(string) *Verdict { return f() } }

func firstMatch(msg string, rules []rule) *Verdict {
	for _, r := range rules {
		if anyIn(msg, r.patterns) {
			return r.verdict(msg)
		}
	}
	return nil
}

var (
	imageToolRules = []rule{
		{multimodalToolContentPatterns, fixed(vMultimodal)}, {imageCorruptPatterns, fixed(vImageCorrupt)},
		{imageTooLargePatterns, fixed(vImageTooLarge)},
	}
	overflowAs5xxRules = []rule{
		{emptyProviderResponsePatterns, fixed(vServerError)}, {memoryCeilingPatterns, fixed(vOverloaded)},
		{contextOverflowPatterns, fixed(vContextOverflow)},
	}
	rules404 = []rule{
		{billingPatterns, fixed(vBilling)}, {providerPolicyBlockedPatterns, fixed(vPolicyBlocked)},
		{modelNotFoundPatterns, fixed(vModelNotFound)},
	}
	tailRules400 = append(append([]rule{}, overflowAs5xxRules...), []rule{
		{providerPolicyBlockedPatterns, fixed(vPolicyBlocked)}, {modelNotFoundPatterns, fixed(vModelNotFound)},
		{rateLimitPatterns, fixed(vRateLimit)}, {billingPatterns, billingHints},
	}...)
	messageHeadRules = append([]rule{
		{memoryCeilingPatterns, fixed(vOverloaded)},
		{payloadTooLargePatterns, fixed(vPayloadTooLarge)},
		{roleAlternationPatterns, fixed(vRoleAlternation)},
		{streamRenderErrorPatterns, fixed(vFormatError)},
	}, imageToolRules...)
	messageTailRules = []rule{
		{overloadedPatterns, fixed(vOverloaded)}, {billingPatterns, billingHints},
		{rateLimitPatterns, fixed(vRateLimit)}, {emptyProviderResponsePatterns, fixed(vServerError)},
		{contextOverflowPatterns, fixed(vContextOverflow)}, {authPatterns, fixed(vAuthRotate)},
		{providerPolicyBlockedPatterns, fixed(vPolicyBlocked)}, {modelNotFoundPatterns, fixed(vModelNotFound)},
		{timeoutMessagePatterns, fixed(vTimeout)}, {connectionMessagePatterns, fixed(vTimeout)},
	}
)

func errorCodeVerdict(code string) *Verdict {
	switch code {
	case "resource_exhausted", "throttled", "rate_limit_exceeded":
		return v(ReasonRateLimit).rotate(true)
	case "model_not_found", "model_not_available", "invalid_model":
		return vModelNotFound()
	case "context_length_exceeded", "max_tokens_exceeded":
		return vContextOverflow()
	case "invalid_encrypted_content":
		return vInvalidEncrypted()
	}
	if billingErrorCodes[code] {
		return vBilling()
	}
	if memoryCeilingErrorCodes[code] {
		return vOverloaded()
	}
	return nil
}

func providerCodeVerdict(family, code string) *Verdict {
	switch family {
	case "openai":
		if code == "server_error" {
			return vServerError()
		}
	case "gemini":
		switch code {
		case "unavailable":
			return vOverloaded()
		case "deadline_exceeded":
			return vTimeout()
		case "internal":
			return vServerError()
		}
	case "anthropic":
		switch code {
		case "api_error":
			return vServerError()
		case "rate_limit_error":
			return vRateLimit()
		}
	}
	return nil
}

// HeaderCarrier exposes the response headers of a failed call for reset and welcome tier parsing
type HeaderCarrier interface{ ResponseHeaders() http.Header }

// ClassifyInput carries everything the classifier stages need about one failed call
type ClassifyInput struct {
	Err           error
	Status        int
	Body          string
	Headers       http.Header
	Provider      string
	Model         string
	BaseURL       string
	APIKey        string
	ApproxTokens  int
	ContextLength int
	NumMessages   int
}

type classifyCtx struct {
	in           ClassifyInput
	err          error
	status       int
	body         map[string]any
	msg          string
	provider     string
	model        string
	baseURL      string
	anonymous    bool
	errorType    string
	errorCode    string
	code         string
	headers      http.Header
	providerSlug string
	modelSlug    string
}

func (c *classifyCtx) largeSession(frac float64, tokens, messages int) bool {
	return float64(c.in.ApproxTokens) > float64(c.in.ContextLength)*frac ||
		(c.in.ContextLength <= 256000 && (c.in.ApproxTokens > tokens || c.in.NumMessages > messages))
}

// ClassifierHook lets a plugin add or correct a verdict before the built in pipeline runs
type ClassifierHook func(in ClassifyInput, status int, errorType, errorCode, message string, body map[string]any) *Verdict

var (
	classifierHooksMu   sync.RWMutex
	classifierHooks     []ClassifierHook
	providerClassifiers = map[string]ClassifierHook{}
)

// RegisterClassifierHook adds a plugin classification hook consulted first in registration order
func RegisterClassifierHook(h ClassifierHook) {
	classifierHooksMu.Lock()
	defer classifierHooksMu.Unlock()
	classifierHooks = append(classifierHooks, h)
}

// RegisterProviderClassifier binds a provider profile verdict hook to its provider slug
func RegisterProviderClassifier(provider string, h ClassifierHook) {
	classifierHooksMu.Lock()
	defer classifierHooksMu.Unlock()
	providerClassifiers[strings.ToLower(strings.TrimSpace(provider))] = h
}

func pluginVerdict(c *classifyCtx) *Verdict {
	classifierHooksMu.RLock()
	hooks := append([]ClassifierHook{}, classifierHooks...)
	classifierHooksMu.RUnlock()
	for _, h := range hooks {
		if vd := h(c.in, c.status, c.errorType, c.errorCode, c.msg, c.body); vd != nil && vd.Reason != "" {
			return vd
		}
	}
	return nil
}

func profileVerdict(c *classifyCtx) *Verdict {
	if c.providerSlug == "" {
		return nil
	}
	classifierHooksMu.RLock()
	h, ok := providerClassifiers[c.providerSlug]
	classifierHooksMu.RUnlock()
	if !ok {
		return nil
	}
	vd := h(c.in, c.status, c.errorType, c.errorCode, c.msg, c.body)
	if vd == nil || vd.Reason == "" {
		return nil
	}
	if vd.ShouldFallback != nil && *vd.ShouldFallback && vd.Retryable == nil && !RetryableClientReasons[vd.Reason] {
		vd.Retryable = bptr(false)
	}
	return vd
}

func nousWelcomeTier(c *classifyCtx) *Verdict {
	status := c.status
	if !c.anonymous {
		if c.provider == "nous" && status == 400 && WelcomeRouteRefusal(status, c.msg, "") == "named_on_welcome_host" {
			return v(ReasonFormatError).retryable(false).fallback(true).ctx(map[string]any{"welcome_route": "named_on_welcome_host"})
		}
		return nil
	}
	if status == 402 && RouteIsWelcomeHost(c.baseURL) {
		refusal := WelcomeRefusal{Reason: "refused", Alternates: []string{}}
		return v(ReasonAuthPermanent).retryable(false).fallback(true).ctx(map[string]any{"welcome_refusal": refusal})
	}
	if status == 429 {
		refusal, ok := ParseWelcomeRefusal(c.body)
		if !ok {
			bodyWait, _ := rateLimitResetSeconds(c.msg, c.body, c.headers)
			refusal = WelcomeRefusalFromHeaders(c.headers, bodyWait)
		}
		ctx := map[string]any{"welcome_refusal": refusal}
		if WelcomeTierGateReasons[refusal.Reason] {
			return v(ReasonModelNotFound).retryable(false).fallback(true).ctx(ctx)
		}
		if refusal.RetryAfter > 0 {
			ctx["reset_at"] = time.Now().Add(time.Duration(refusal.RetryAfter) * time.Second)
		}
		return v(ReasonRateLimit).fallback(true).ctx(ctx)
	}
	plain403 := !anyIn(c.msg, welcome403NamedPatterns)
	routeURL := ""
	if plain403 {
		routeURL = c.baseURL
	}
	kind := WelcomeRouteRefusal(status, c.msg, routeURL)
	if kind == "" {
		return nil
	}
	ctx := map[string]any{"welcome_route": kind}
	if status == 403 {
		return v(ReasonAuthPermanent).retryable(false).fallback(true).ctx(ctx)
	}
	return v(ReasonFormatError).retryable(false).fallback(true).ctx(ctx)
}

func isAnthropicThinkingRejection(msg string) bool {
	return strings.Contains(msg, "thinking") && (anyIn(msg, thinkingMutationWords) ||
		(strings.Contains(msg, "redacted_thinking") && strings.Contains(msg, "invalid")))
}

func providerSpecialCases(c *classifyCtx) *Verdict {
	msg, status := c.msg, c.status
	if c.code == "data_policy_denied" {
		return v(ReasonDataPolicyDenied).retryable(false).fallback(false)
	}
	if welcome := nousWelcomeTier(c); welcome != nil {
		return welcome
	}
	if anyIn(msg, contentPolicyBlockedPatterns) {
		return vContentBlocked()
	}
	if anyIn(msg, accountPolicyBlockPatterns) {
		return vPolicyBlocked()
	}
	if isCodexMaskedReplayRejection(c) {
		return v(ReasonInvalidEncryptedContent).abortFallback()
	}
	if status == 400 && (c.code == "thinking_signature_invalid" || strings.Contains(msg, "thinking_signature_invalid")) {
		return vInvalidEncrypted()
	}
	if status == 400 && isAnthropicThinkingRejection(msg) {
		return v(ReasonThinkingSignature)
	}
	if status == 429 && strings.Contains(msg, "extra usage") && strings.Contains(msg, "long context") {
		return v(ReasonLongContextTier).compress(true)
	}
	if status == 400 && strings.Contains(msg, "long context beta") && strings.Contains(msg, "not yet available") {
		return v(ReasonOAuthLongContextBetaForbid)
	}
	grammarHit := strings.Contains(msg, "error parsing grammar") || strings.Contains(msg, "json-schema-to-grammar") ||
		(strings.Contains(msg, "unable to generate parser") && strings.Contains(msg, "template")) ||
		(strings.Contains(msg, "invalid json schema") && strings.Contains(msg, "regex lookaround") && strings.Contains(msg, "not supported"))
	if status == 400 && grammarHit && !strings.Contains(msg, noUserQuerySignal) {
		return v(ReasonLlamaCppGrammarPattern)
	}
	if strings.Contains(msg, "do not have an active grok subscription") || (strings.Contains(msg, "out of available resources") && strings.Contains(msg, "grok")) {
		return vAuthFallback()
	}
	return nil
}

func moaSpecialCases(c *classifyCtx) *Verdict {
	var preset *MoAPresetNotFoundError
	if errors.As(c.err, &preset) {
		return v(ReasonModelNotFound).retryable(false)
	}
	return nil
}

func byErrorCode(c *classifyCtx) *Verdict {
	if c.code == ProviderStreamNonJSONErrorCode && strings.Contains(c.msg, "request validation failed:") {
		return vFormatError()
	}
	if vd := errorCodeVerdict(c.code); vd != nil {
		return vd
	}
	family, ok := providerCodeFamilies[c.providerSlug]
	if !ok {
		family = c.providerSlug
	}
	return providerCodeVerdict(family, c.code)
}

func byMessage(c *classifyCtx) *Verdict {
	if head := firstMatch(c.msg, messageHeadRules); head != nil {
		return head
	}
	if anyIn(c.msg, usageLimitPatterns) {
		return classify402(c.msg, "", nil)
	}
	return firstMatch(c.msg, messageTailRules)
}

func byTransport(c *classifyCtx) *Verdict {
	msg := c.msg
	if ssl := firstMatch(msg, []rule{{sslCertVerifyPatterns, fixed(vSSLCert)}, {sslTransientPatterns, fixed(vTimeout)}}); ssl != nil {
		return ssl
	}
	if isCertificateError(c.err) {
		return vSSLCert()
	}
	if anyIn(msg, serverDisconnectPatterns) && c.status == 0 {
		if _, ok := ReasoningStaleTimeoutFloor(c.model); ok {
			return vTimeout()
		}
		if c.largeSession(0.6, 120000, 200) {
			return vContextOverflow()
		}
		return vTimeout()
	}
	if strings.Contains(msg, "consecutive stale attempts") && strings.Contains(msg, "aborting this call") {
		return v(ReasonTimeout).abortFallback()
	}
	if isTransportError(c.err) {
		return vTimeout()
	}
	return nil
}

func byStatus(c *classifyCtx) *Verdict {
	status := c.status
	if status == 0 {
		return nil
	}
	switch status {
	case 400:
		return classify400(c)
	case 401:
		return vAuthRotate()
	case 402:
		code := ""
		if isNousRoute(c) {
			code = c.code
		}
		return classify402(c.msg, code, c.body)
	case 403:
		return status403(c)
	case 404:
		return status404(c)
	case 408:
		return vTimeout()
	case 413:
		return vPayloadTooLarge()
	case 422:
		return classifyImageTool422(c)
	case 429:
		return status429(c)
	case 500, 502:
		return status5xx(c)
	case 503, 529:
		if vd := firstMatch(c.msg, overflowAs5xxRules); vd != nil {
			return vd
		}
		return vOverloaded()
	}
	if status >= 400 && status < 500 {
		return vFormatError()
	}
	if status >= 500 && status < 600 {
		return vServerError()
	}
	return nil
}

func status403(c *classifyCtx) *Verdict {
	if transient403Codes[c.code] {
		return vOverloaded()
	}
	xaiSpend := c.providerSlug == "xai-oauth" && c.code == xaiSpendingLimitErrorCode
	if xaiSpend || anyIn(c.msg, append([]string{"key limit exceeded", "spending limit"}, billingPatterns...)) {
		return vBilling()
	}
	if anyIn(c.msg, upstreamBlockedPatterns) {
		return vUpstreamBlocked()
	}
	return vAuthFallback()
}

func status404(c *classifyCtx) *Verdict {
	if billingErrorCodes[c.code] {
		return vBilling()
	}
	if vd := firstMatch(c.msg, rules404); vd != nil {
		return vd
	}
	if modelIDMissingKnownPrefix(c.modelSlug, c.providerSlug) {
		return vModelNotFound()
	}
	return vUnknown()
}

func status429(c *classifyCtx) *Verdict {
	if billingErrorCodes[c.code] {
		return vBilling()
	}
	if anyIn(c.msg, overloadedPatterns) {
		return vOverloaded()
	}
	if isOpenRouterUpstreamError(c.body, c.providerSlug) {
		ctx := map[string]any{}
		if upstream := extractUpstreamProviderName(c.body); upstream != "" {
			ctx["upstream_provider"] = upstream
		}
		return v(ReasonUpstreamRateLimit).fallback(true).ctx(ctx)
	}
	quotaWall := c.code == "usage_limit_reached" || anyIn(c.msg, append(append([]string{"usage_limit_reached"}, usageLimitPatterns...), billingPatterns...))
	explicitRateLimit := anyIn(c.msg, rateLimitPatterns)
	if quotaWall && !explicitRateLimit && !hasUsageLimitTransientSignal(c.msg, c.body, c.headers) {
		return vBilling()
	}
	if reset, ok := rateLimitResetSeconds(c.msg, c.body, c.headers); ok && reset > 0 {
		return vRateLimit().ctx(map[string]any{"reset_at": time.Now().Add(reset)})
	}
	return vRateLimit()
}

func status5xx(c *classifyCtx) *Verdict {
	validation := anyIn(c.msg, requestValidationPatterns) || validation5xxCodes[c.code]
	if validation && !isServerInjectedParamRejection(c.msg, c.providerSlug) {
		return vFormatError()
	}
	if vd := firstMatch(c.msg, overflowAs5xxRules); vd != nil {
		return vd
	}
	return vServerError()
}

func isNousRoute(c *classifyCtx) bool {
	if c.providerSlug == "nous" {
		return true
	}
	return c.baseURL != "" && baseURLUnderDomain(c.baseURL, "nousresearch.com")
}

func classify402(msg, code string, body map[string]any) *Verdict {
	if code == nousUsageWindow402Code {
		ctx := map[string]any{"usage_window": true}
		if display := usageWindowDisplayMessage(body); display != "" {
			ctx["display_message"] = display
		}
		return vBilling().fallback(false).ctx(ctx)
	}
	transient := anyIn(msg, usageLimitPatterns) && anyIn(msg, usageLimitTransientSignals)
	if transient {
		return vRateLimit()
	}
	return vBilling()
}

func usageWindowDisplayMessage(body map[string]any) string {
	if body == nil {
		return ""
	}
	holders := []map[string]any{body}
	if nested, ok := body["error"].(map[string]any); ok {
		holders = append(holders, nested)
	}
	for _, h := range holders {
		if s, ok := h["display_message"].(string); ok && strings.TrimSpace(s) != "" {
			return truncate(strings.TrimSpace(s), 500)
		}
	}
	return ""
}

// Image parts over this size are rewritten by the shrink pass so a rejection carrying one is a size cap
const imageShrinkTargetBytes = 4 * 1024 * 1024

func hasLargeInlineImage(content any) bool {
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		pm, ok := part.(map[string]any)
		if !ok {
			continue
		}
		image := pm["image_url"]
		u := ""
		switch im := image.(type) {
		case map[string]any:
			u, _ = im["url"].(string)
		case string:
			u = im
		}
		if strings.HasPrefix(u, "data:image/") && len(u) > imageShrinkTargetBytes {
			return true
		}
	}
	return false
}

func oversizedMessageContentRejection(body map[string]any) bool {
	details, ok := body["detail"].([]any)
	if !ok {
		return false
	}
	for _, d := range details {
		detail, ok := d.(map[string]any)
		if !ok {
			continue
		}
		loc, ok := detail["loc"].([]any)
		if detail["type"] != "string_type" || !ok || len(loc) < 2 {
			continue
		}
		parts := make([]string, len(loc))
		for i, x := range loc {
			parts[i] = strings.ToLower(fmt.Sprint(x))
		}
		tool := false
		for _, x := range parts {
			if strings.HasPrefix(x, "tool") {
				tool = true
			}
		}
		if parts[0] == "body" && parts[1] == "messages" && parts[len(parts)-2] == "content" && parts[len(parts)-1] == "str" && !tool && hasLargeInlineImage(detail["input"]) {
			return true
		}
	}
	return false
}

func classify400(c *classifyCtx) *Verdict {
	msg, code := c.msg, c.code
	if oversizedMessageContentRejection(c.body) {
		return vImageTooLarge()
	}
	if vd := firstMatch(msg, imageToolRules); vd != nil {
		return vd
	}
	if strings.Contains(msg, CodexAccountModelEntitlementMarker) {
		return vModelEntitlement()
	}
	if code == "invalid_encrypted_content" || strings.Contains(msg, "invalid_encrypted_content") ||
		(strings.Contains(msg, "encrypted content for item") && strings.Contains(msg, "could not be verified")) ||
		strings.Contains(msg, "could not decrypt the provided encrypted_content") ||
		strings.Contains(msg, "encrypted content could not be decrypted or parsed") ||
		(strings.Contains(msg, "encrypted_content") && strings.Contains(msg, "was not issued to this caller")) ||
		strings.Contains(msg, "conflicting authenticated continuation identities") {
		return vInvalidEncrypted()
	}
	if strings.Contains(msg, reasoningMandatoryPattern) || IsReasoningFieldRejection(msg) || IsReasoningLevelRejection(msg) {
		return vReasoningMandatory()
	}
	if isServerInjectedParamRejection(msg, c.providerSlug) {
		return vServerError()
	}
	if anyIn(msg, malformedToolArgsPatterns) {
		return vMalformedToolArgs()
	}
	if anyIn(msg, roleAlternationPatterns) {
		return vRoleAlternation()
	}
	if anyIn(msg, validation400Patterns) || validation400Codes[code] {
		return vFormatError()
	}
	if anyIn(msg, invalidMessageBodyPatterns) || code == "invalid_request_body" {
		return vFormatError()
	}
	if memoryCeilingErrorCodes[code] {
		return vOverloaded()
	}
	if vd := firstMatch(msg, tailRules400); vd != nil {
		return vd
	}
	bodyMsg := ""
	for _, cand := range bodyMessageCandidates(c.body) {
		if s := strings.ToLower(strings.TrimSpace(cand)); s != "" {
			bodyMsg = s
			break
		}
	}
	isGeneric := len(bodyMsg) < 30 || bodyMsg == "error"
	if isGeneric && c.largeSession(0.4, 80000, 80) {
		return vContextOverflow()
	}
	return vFormatError()
}

func classifyImageTool422(c *classifyCtx) *Verdict {
	if oversizedMessageContentRejection(c.body) {
		return vImageTooLarge()
	}
	if vd := firstMatch(c.msg, imageToolRules); vd != nil {
		return vd
	}
	return vFormatError()
}

func hasUsageLimitTransientSignal(msg string, body map[string]any, headers http.Header) bool {
	if anyIn(msg, usageLimitTransientSignals) {
		return true
	}
	for _, payload := range []map[string]any{body, errorObj(body)} {
		for _, f := range resetFields {
			if val, ok := payload[f]; ok && val != nil && val != "" {
				return true
			}
		}
	}
	for _, h := range resetHeaders {
		if headers.Get(h) != "" {
			return true
		}
	}
	return false
}

func rateLimitResetSeconds(msg string, body map[string]any, headers http.Header) (time.Duration, bool) {
	for _, payload := range []map[string]any{body, errorObj(body)} {
		for _, name := range resetFields {
			val, ok := payload[name]
			if !ok || val == nil || val == "" {
				continue
			}
			if f, isNum := val.(float64); isNum && strings.HasSuffix(name, "_at") {
				d := time.Until(time.Unix(0, int64(f*float64(time.Second))))
				if d < 0 {
					d = 0
				}
				return d, true
			}
			if d, ok := retryAfterValue(val); ok {
				return d, true
			}
		}
	}
	if d, ok := RetryAfterFromHeaders(headers); ok {
		return d, true
	}
	return ResetDelayFromMessage(msg)
}

func modelIDMissingKnownPrefix(model, provider string) bool {
	name := strings.TrimSpace(model)
	if name == "" || strings.Contains(name, "/") {
		return false
	}
	return SuggestPrefixedModelID(strings.TrimSpace(provider), name) != ""
}

func isServerInjectedParamRejection(msg, provider string) bool {
	slug := strings.ToLower(strings.TrimSpace(provider))
	for param, senders := range serverInjectedParamSenders {
		if msg != "" && strings.Contains(msg, param) && anyIn(msg, paramRejectionWords) {
			for _, sender := range senders {
				if strings.Contains(slug, sender) {
					return false
				}
			}
			return true
		}
	}
	return false
}

const (
	codexMaskedReplayMessage      = "request blocked."
	codexUnsupportedContentDetail = "unsupported content type"
)

func isCodexMaskedReplayRejection(c *classifyCtx) bool {
	if c.providerSlug != "openai-codex" || (c.status != 0 && c.status != 400) {
		return false
	}
	body := c.body
	if detail, _ := body["detail"].(string); strings.ToLower(strings.TrimSpace(detail)) == codexUnsupportedContentDetail ||
		(len(body) == 0 && strings.Contains(c.msg, codexUnsupportedContentDetail) && strings.Contains(c.msg, "detail")) {
		return true
	}
	bodyMsg := ""
	for _, cand := range bodyMessageCandidates(body) {
		if cand != "" {
			bodyMsg = strings.ToLower(strings.TrimSpace(cand))
			break
		}
	}
	return (c.code == "invalid_prompt" && bodyMsg == codexMaskedReplayMessage) ||
		strings.TrimSpace(c.msg) == "invalid_prompt: "+codexMaskedReplayMessage
}

func errorObj(body map[string]any) map[string]any {
	if e, ok := body["error"].(map[string]any); ok {
		return e
	}
	return map[string]any{}
}

func decodeJSONObject(text string) map[string]any {
	text = strings.TrimSpace(strings.TrimPrefix(text, "\xef\xbb\xbf"))
	if text == "" || text[0] != '{' {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil
	}
	return out
}

func openRouterWrappedMessage(errObj map[string]any) string {
	metadata, ok := errObj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	raw, _ := metadata["raw"].(string)
	inner := decodeJSONObject(raw)
	if inner == nil {
		return ""
	}
	m, _ := errorObj(inner)["message"].(string)
	return strings.ToLower(m)
}

func stringField(m map[string]any, key string) string {
	switch val := m[key].(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		return fmt.Sprint(val)
	}
}

func buildErrorMsg(raw string, body map[string]any, bodyText string) string {
	rawMsg := strings.ToLower(raw)
	parts := []string{rawMsg}
	bodyMsg, metadataMsg := "", ""
	if body != nil {
		eo := errorObj(body)
		bodyMsg = strings.ToLower(stringField(eo, "message"))
		if bodyMsg == "" {
			bodyMsg = strings.ToLower(stringField(body, "message"))
		}
		if len(eo) > 0 {
			metadataMsg = openRouterWrappedMessage(eo)
		}
	} else if t := strings.ToLower(strings.TrimSpace(bodyText)); t != "" && !strings.Contains(rawMsg, t) {
		parts = append(parts, truncate(t, 8000))
	}
	if bodyMsg != "" && !strings.Contains(rawMsg, bodyMsg) {
		parts = append(parts, bodyMsg)
	}
	if metadataMsg != "" && !strings.Contains(rawMsg, metadataMsg) && !strings.Contains(bodyMsg, metadataMsg) {
		parts = append(parts, metadataMsg)
	}
	return strings.Join(parts, " ")
}

func bodyMessageCandidates(body map[string]any) []string {
	if body == nil {
		return nil
	}
	out := []string{stringField(errorObj(body), "message"), stringField(body, "message"), stringField(body, "errorMessage")}
	if args, ok := body["errorArgs"].(map[string]any); ok {
		out = append(out, stringField(args, "reason"))
	}
	switch detail := body["detail"].(type) {
	case map[string]any:
		out = append(out, stringField(detail, "message"))
	case string:
		out = append(out, detail)
	}
	return out
}

func statusCodeFromBody(body map[string]any) int {
	if body == nil {
		return 0
	}
	eo := errorObj(body)
	candidates := []any{eo["status_code"], eo["status"], eo["http_status"], eo["code"], body["code"]}
	for _, c := range candidates {
		if f, ok := c.(float64); ok && f == float64(int(f)) && f >= 400 && f < 600 {
			return int(f)
		}
	}
	return 0
}

func codeFromPayload(payload map[string]any, topKeys []string, peekMessage bool) string {
	if payload == nil {
		return ""
	}
	if eo, ok := payload["error"].(map[string]any); ok {
		var code any = eo["code"]
		if code == nil || code == "" {
			code = eo["type"]
		}
		if _, isStr := code.(string); !isStr {
			if st, ok := eo["status"]; ok && st != nil {
				code = st
			}
		}
		if s, ok := code.(string); ok && strings.TrimSpace(s) != "" && strings.TrimSpace(s) != "400" {
			return strings.TrimSpace(s)
		}
		if message, ok := eo["message"].(string); peekMessage && ok && strings.HasPrefix(strings.TrimSpace(message), "{") {
			if nested := codeFromPayload(decodeJSONObject(message), []string{"code", "error_code"}, false); nested != "" {
				return nested
			}
		}
	}
	for _, k := range topKeys {
		val := payload[k]
		if val == nil || val == "" || val == false {
			continue
		}
		text := ""
		switch t := val.(type) {
		case string:
			text = strings.TrimSpace(t)
		case float64:
			text = strconv.FormatFloat(t, 'f', -1, 64)
		}
		if text != "" && text != "400" {
			return text
		}
		break
	}
	return ""
}

func extractErrorCode(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	return codeFromPayload(body, []string{"code", "error_code", "errorCode"}, true)
}

func extractMessage(raw string, body map[string]any) string {
	for _, m := range bodyMessageCandidates(body) {
		if strings.TrimSpace(m) != "" {
			return truncate(strings.TrimSpace(m), 500)
		}
	}
	return truncate(raw, 500)
}

func isOpenRouterUpstreamError(body map[string]any, provider string) bool {
	eo := errorObj(body)
	if strings.ToLower(strings.TrimSpace(stringField(eo, "message"))) != "provider returned error" {
		return false
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "openrouter" {
		return true
	}
	metadata, ok := eo["metadata"].(map[string]any)
	if !ok {
		return false
	}
	_, hasRaw := metadata["raw"]
	_, hasName := metadata["provider_name"]
	return hasRaw || hasName
}

func extractUpstreamProviderName(body map[string]any) string {
	metadata, ok := errorObj(body)["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := metadata["provider_name"].(string)
	return strings.TrimSpace(name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func hostOfBaseURL(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func baseURLUnderDomain(baseURL, domain string) bool {
	host := hostOfBaseURL(baseURL)
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func offRouteHost(c *classifyCtx) string {
	host := hostOfBaseURL(c.baseURL)
	if host == "" {
		return ""
	}
	if owned, known := ProviderOwnsRoute(c.providerSlug, c.baseURL); known && owned {
		return ""
	}
	return host
}

// Go error chain heuristics standing in for the Python transport exception type names
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var dns *net.DNSError
	var op *net.OpError
	var ue *url.Error
	var pe *os.PathError
	var se *os.SyscallError
	return errors.As(err, &dns) || errors.As(err, &op) || errors.As(err, &ue) || errors.As(err, &pe) || errors.As(err, &se)
}

func isCertificateError(err error) bool {
	if err == nil {
		return false
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var ce x509.CertificateInvalidError
	var he x509.HostnameError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &ce) || errors.As(err, &he)
}

func applyVerdict(out *Classified, vd *Verdict) {
	out.Reason = vd.Reason
	if vd.Retryable != nil {
		out.Retryable = *vd.Retryable
	}
	if vd.ShouldCompress != nil {
		out.ShouldCompress = *vd.ShouldCompress
	}
	if vd.ShouldRotateCredential != nil {
		out.ShouldRotateCredential = *vd.ShouldRotateCredential
	}
	if vd.ShouldFallback != nil {
		out.ShouldFallback = *vd.ShouldFallback
	}
	if vd.Context != nil {
		out.Context = vd.Context
	}
}

// ClassifyError runs the staged pipeline over one failed call and returns its verdict with recovery hints
func ClassifyError(in ClassifyInput) *Classified {
	if in.ContextLength <= 0 {
		in.ContextLength = 200000
	}
	var apiErr *APIError
	raw := ""
	status := in.Status
	body := decodeJSONObject(in.Body)
	bodyText := in.Body
	headers := in.Headers
	if errors.As(in.Err, &apiErr) {
		if status == 0 {
			status = apiErr.StatusCode
		}
		if body == nil {
			body = decodeJSONObject(apiErr.Body)
			if bodyText == "" {
				bodyText = apiErr.Body
			}
		}
		raw = apiErr.Message
		if raw == "" && apiErr.Err != nil {
			raw = apiErr.Err.Error()
		}
		if in.Provider == "" {
			in.Provider = apiErr.Provider
		}
		if in.Model == "" {
			in.Model = apiErr.Model
		}
	} else if in.Err != nil {
		raw = in.Err.Error()
	}
	if headers == nil {
		var hc HeaderCarrier
		if errors.As(in.Err, &hc) {
			headers = hc.ResponseHeaders()
		}
	}
	if status == 0 && body != nil {
		status = statusCodeFromBody(body)
	}
	out := &Classified{StatusCode: status, Provider: in.Provider, Model: in.Model, Retryable: true, Context: map[string]any{}}
	if in.Err != nil && errors.Is(in.Err, context.Canceled) {
		out.Reason, out.Retryable, out.Message = ReasonCancelled, false, "request cancelled"
		return out
	}
	c := &classifyCtx{
		in: in, err: in.Err, status: status, body: body, msg: buildErrorMsg(raw, body, bodyText),
		provider: in.Provider, model: in.Model, baseURL: in.BaseURL,
		anonymous: IsAnonymousRequest(in.Provider, in.APIKey), headers: headers,
		providerSlug: strings.ToLower(strings.TrimSpace(in.Provider)), modelSlug: strings.ToLower(strings.TrimSpace(in.Model)),
	}
	if in.Err != nil {
		c.errorType = fmt.Sprintf("%T", in.Err)
	}
	c.errorCode = extractErrorCode(body)
	c.code = strings.ToLower(c.errorCode)
	stages := []func(*classifyCtx) *Verdict{pluginVerdict, profileVerdict, providerSpecialCases, byStatus, moaSpecialCases, byErrorCode, byMessage, byTransport}
	var verdict *Verdict
	for _, stage := range stages {
		if verdict = stage(c); verdict != nil {
			break
		}
	}
	if verdict == nil {
		verdict = vUnknown()
	}
	applyVerdict(out, verdict)
	message := extractMessage(raw, body)
	if out.IsAuth() {
		if host := offRouteHost(c); host != "" {
			message = message + " (endpoint: " + host + ")"
		}
	}
	out.Message = message
	return out
}

// KindFor maps a failover reason onto the coarse error kind adapters and views read
func KindFor(reason FailoverReason) ErrorKind {
	switch reason {
	case ReasonAuth, ReasonAuthPermanent, ReasonUpstreamBlocked:
		return ErrAuth
	case ReasonBilling:
		return ErrQuota
	case ReasonRateLimit, ReasonUpstreamRateLimit:
		return ErrRateLimit
	case ReasonOverloaded, ReasonServerError:
		return ErrServer
	case ReasonTimeout:
		return ErrTimeout
	case ReasonSSLCertVerification:
		return ErrNetwork
	case ReasonContextOverflow, ReasonPayloadTooLarge, ReasonLongContextTier:
		return ErrContextOverflow
	case ReasonModelNotFound, ReasonModelEntitlement:
		return ErrModelNotFound
	case ReasonContentPolicyBlocked:
		return ErrContentFilter
	case ReasonIncompleteResponse:
		return ErrEmptyResponse
	case ReasonCancelled:
		return ErrCancelled
	case ReasonUnknown:
		return ErrUnknown
	}
	return ErrInvalidRequest
}

// Classify builds an APIError whose Kind drives retry and fallback decisions
func Classify(provider, model string, status int, body string, err error) *APIError {
	var existing *APIError
	if errors.As(err, &existing) && existing.Provider == provider && existing.Model == model && existing.StatusCode == status && existing.Kind != "" && existing.Kind != ErrUnknown {
		return existing
	}
	cl := ClassifyError(ClassifyInput{Err: err, Status: status, Body: body, Provider: provider, Model: model})
	e := &APIError{Provider: provider, Model: model, StatusCode: cl.StatusCode, Body: body, Err: err, Kind: KindFor(cl.Reason), Message: cl.Message}
	if bodyMap := decodeJSONObject(body); bodyMap != nil {
		e.Code = extractErrorCode(bodyMap)
		e.Type = stringField(errorObj(bodyMap), "type")
		if d, ok := retryAfterValue(errorObj(bodyMap)["retry_after"]); ok && d > 0 {
			e.RetryAfter = d
		} else if d, ok := retryAfterValue(bodyMap["retry_after"]); ok && d > 0 {
			e.RetryAfter = d
		}
	}
	if at, ok := cl.ResetAt(); ok && e.RetryAfter == 0 {
		if d := time.Until(at); d > 0 {
			e.RetryAfter = d
		}
	}
	if e.Message == "" {
		e.Message = firstLine(body)
	}
	return e
}

// RetryAfterFrom parses a Retry-After header value in seconds or HTTP date form
func RetryAfterFrom(value string) time.Duration {
	d, ok := ParseRetryAfter(value)
	if !ok {
		return 0
	}
	return d
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 400)
}

var reasoningStaleTimeoutFloors = map[int][]string{
	600: {"nemotron-3-ultra", "nemotron-3-super", "nemotron-3.5-super-vl",
		"deepseek-r1", "deepseek-reasoner", "deepseek-flash", "deepseek-v4-flash", "deepseek-v4.1-flash", "deepseek-v4-pro",
		"o1", "o1-mini", "o1-pro", "o1-preview", "o3", "o3-pro", "gpt-5.6", "gpt-6", "claude-fable", "gemini-4"},
	300: {"nemotron-3-nano", "nemotron-3.5-lightning", "qwq-32b", "o3-mini", "o4-mini",
		"grok-4-fast-reasoning", "grok-4.20-reasoning", "grok-4.5", "grok-4.6", "ox-alpha", "x-preview-f-free", "inkling", "minimax-m2"},
	240: {"claude-opus-4", "claude-opus-5"},
	180: {"qwen3", "claude-sonnet-5", "claude-sonnet-4.5", "claude-sonnet-4.6", "grok-4-fast-non-reasoning"},
}

type slugFloor struct {
	slug  string
	floor time.Duration
	re    *regexp.Regexp
}

var sortedReasoningFloors = func() []slugFloor {
	var out []slugFloor
	for floor, slugs := range reasoningStaleTimeoutFloors {
		for _, slug := range slugs {
			out = append(out, slugFloor{slug, time.Duration(floor) * time.Second, regexp.MustCompile(`^` + regexp.QuoteMeta(slug) + `(?:$|[\-._:])`)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].slug) > len(out[j].slug) })
	return out
}()

// ReasoningStaleTimeoutFloor returns the stale detector floor for a known reasoning model slug
func ReasoningStaleTimeoutFloor(model string) (time.Duration, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return 0, false
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	for _, sf := range sortedReasoningFloors {
		if sf.re.MatchString(name) {
			return sf.floor, true
		}
	}
	return 0, false
}

// Substrings of the SSE JSON parser error for a truncated or corrupted event stream frame
var ProviderStreamParseMarkers = []string{
	"expected ident at line", "expected value at line", "eof while parsing a value at line",
	"eof while parsing a string at line", "eof while parsing a list at line", "eof while parsing an object at line",
	"key must be a string at line", "trailing characters at line", "trailing comma at line",
	"expected `,` or `}` at line", "expected `,` or `]` at line", "expected `:` at line",
	"invalid escape at line", "invalid number at line", "found while parsing a string at line",
}

// IsProviderStreamParseError reports a wire level stream frame parse failure rather than local JSON validation
func IsProviderStreamParseError(err error) bool {
	if err == nil {
		return false
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var apiErr *APIError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) || errors.As(err, &apiErr) {
		return false
	}
	return anyIn(strings.ToLower(err.Error()), ProviderStreamParseMarkers)
}

// Offline DNS failure wording that transports wrap in a generic connection error
var NetworkResolutionMarkers = []string{
	"temporary failure in name resolution", "name or service not known",
	"nodename nor servname provided, or not known", "getaddrinfo failed",
	"no address associated with hostname", "network is unreachable",
	"no such host", "server misbehaving",
}

// OfflineSummary is the one line shown when the provider host cannot be resolved
const OfflineSummary = "Hermes can't reach the model provider. You may be offline. Check your internet connection and try again."

// XAIEntitlementHint is appended to xAI's permission denied body so Premium+ holders learn why
const XAIEntitlementHint = " — xAI rejected this OAuth account. NOTE: X Premium+ does NOT " +
	"include xAI API access — only standalone SuperGrok subscribers " +
	"can use this provider. Other possible causes: no Grok " +
	"subscription, your tier doesn't include this model, or your " +
	"quota is exhausted. Check https://grok.com/?_s=usage to see " +
	"which, or run `/model` to switch providers."

var errorDetailKeys = []string{"message", "detail", "error", "code", "type"}

func isXAIEntitlementText(lower string) bool {
	return strings.Contains(lower, "do not have an active grok subscription") ||
		(strings.Contains(lower, "out of available resources") && strings.Contains(lower, "grok")) ||
		(strings.Contains(lower, "does not have permission") && strings.Contains(lower, "grok"))
}

// IsNetworkResolutionFailure walks the unwrap chain for DNS or unreachable network failures
func IsNetworkResolutionFailure(err error) bool {
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		var dnsErr *net.DNSError
		if errors.As(cur, &dnsErr) && dnsErr == cur {
			return true
		}
		if anyIn(strings.ToLower(cur.Error()), NetworkResolutionMarkers) {
			return true
		}
	}
	return false
}

// IsEntitlementFailure detects subscription 401 or 403 bodies that a token refresh cannot fix
func IsEntitlementFailure(errorContext map[string]any, status int) bool {
	if status != 0 && status != 401 && status != 403 {
		return false
	}
	if errorContext == nil {
		return false
	}
	parts := make([]string, 0, 4)
	for _, k := range []string{"message", "reason", "code", "error"} {
		if v, ok := errorContext[k]; ok && v != nil {
			parts = append(parts, strings.ToLower(fmt.Sprint(v)))
		} else {
			parts = append(parts, "")
		}
	}
	haystack := strings.Join(parts, " ")
	if strings.TrimSpace(haystack) == "" {
		return false
	}
	if strings.Contains(haystack, "[wke=unauthenticated:") || strings.Contains(haystack, "oauth2 access token could not be validated") {
		return false
	}
	return isXAIEntitlementText(haystack)
}

// DecorateXAIEntitlementError appends the Premium+ hint once to an xAI permission denied detail
func DecorateXAIEntitlementError(detail string) string {
	if detail == "" || !isXAIEntitlementText(strings.ToLower(detail)) {
		return detail
	}
	if strings.Contains(detail, "X Premium+ does NOT include") {
		return detail
	}
	return detail + XAIEntitlementHint
}

// CoerceAPIErrorDetail renders a structured provider error field as display text
func CoerceAPIErrorDetail(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		for _, key := range errorDetailKeys {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
		for _, key := range errorDetailKeys {
			if nested, ok := v[key]; ok {
				if d := CoerceAPIErrorDetail(nested); d != "" {
					return d
				}
			}
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(raw)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if p := CoerceAPIErrorDetail(item); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "; ")
	}
	return fmt.Sprint(value)
}

// Transport timeout phases an adapter may stamp on an error
const (
	TimeoutPhaseConnect = "connect"
	TimeoutPhaseRead    = "read"
	TimeoutPhaseWrite   = "write"
	TimeoutPhasePool    = "pool"
)

// TimeoutPhaseError marks a transport timeout with the phase the adapter observed
type TimeoutPhaseError struct {
	Phase string
	Err   error
}

func (e *TimeoutPhaseError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return "Request timed out."
}

func (e *TimeoutPhaseError) Unwrap() error { return e.Err }

// Timeout marks the error as a net.Error timeout
func (e *TimeoutPhaseError) Timeout() bool { return true }

// TimeoutPhase reports connect or read for a timeout in the cause chain, empty for other errors
func TimeoutPhase(err error) string {
	phase := ""
	seen := map[error]bool{}
	for cur := err; cur != nil && !seen[cur]; cur = errors.Unwrap(cur) {
		seen[cur] = true
		switch e := cur.(type) {
		case *APIError:
			continue
		case *TimeoutPhaseError:
			switch e.Phase {
			case TimeoutPhaseConnect:
				return TimeoutPhaseConnect
			case TimeoutPhaseWrite, TimeoutPhasePool:
				return ""
			}
			phase = TimeoutPhaseRead
			continue
		case *net.OpError:
			if !e.Timeout() {
				return phase
			}
			switch e.Op {
			case "dial":
				return TimeoutPhaseConnect
			case "write":
				return ""
			}
			phase = TimeoutPhaseRead
			continue
		}
		if cur == context.DeadlineExceeded || cur == os.ErrDeadlineExceeded {
			return TimeoutPhaseRead
		}
		if ne, ok := cur.(net.Error); ok && ne.Timeout() {
			phase = TimeoutPhaseRead
			continue
		}
		break
	}
	return phase
}

// SummarizeTimeoutError names the timeout phase and the setting to raise, false for other errors
func SummarizeTimeoutError(err error, provider string) (string, bool) {
	switch TimeoutPhase(err) {
	case TimeoutPhaseConnect:
		return "Connection to the provider timed out before it was established (check the base_url and network path)", true
	case TimeoutPhaseRead:
		key := strings.TrimSpace(provider)
		if key == "" {
			key = "<provider>"
		}
		return "Request timed out waiting for the provider to respond (if the model needs longer, raise providers." + key + ".request_timeout_seconds in config.yaml)", true
	}
	return "", false
}

var (
	htmlTitleRe = regexp.MustCompile(`(?i)<title[^>]*>([^<]+)</title>`)
	cfRayRe     = regexp.MustCompile(`Cloudflare Ray ID:\s*<strong[^>]*>([^<]+)</strong>`)
)

func httpPrefix(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode > 0 {
		return fmt.Sprintf("HTTP %d: ", apiErr.StatusCode)
	}
	return ""
}

func rawErrorText(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Message != "" {
			return apiErr.Message
		}
		if apiErr.Body != "" {
			return apiErr.Body
		}
		if apiErr.Err != nil {
			return apiErr.Err.Error()
		}
		return ""
	}
	return err.Error()
}

func summarizeHTMLPage(raw, prefix string) string {
	title := "HTML error page (title not found)"
	if m := htmlTitleRe.FindStringSubmatch(raw); m != nil {
		title = strings.TrimSpace(m[1])
	}
	parts := []string{}
	if prefix != "" {
		parts = append(parts, strings.TrimSuffix(prefix, ": "))
	}
	parts = append(parts, title)
	if m := cfRayRe.FindStringSubmatch(raw); m != nil {
		parts = append(parts, "Ray "+strings.TrimSpace(m[1]))
	}
	return strings.Join(parts, " — ")
}

// SummarizeAPIError extracts a human readable one liner from a provider failure
func SummarizeAPIError(err error, provider string) string {
	if err == nil {
		return ""
	}
	raw := rawErrorText(err)
	if IsNetworkResolutionFailure(err) {
		return OfflineSummary
	}
	if summary, ok := SummarizeTimeoutError(err, provider); ok {
		return summary
	}
	if IsProviderStreamParseError(err) {
		return "Malformed provider streaming response: " + truncate(raw, 300)
	}
	prefix := httpPrefix(err)
	var apiErr *APIError
	body := ""
	if errors.As(err, &apiErr) {
		body = apiErr.Body
	}
	if strings.Contains(raw, "<!DOCTYPE") || strings.Contains(raw, "<html") {
		return summarizeHTMLPage(raw, prefix)
	}
	if strings.Contains(body, "<!DOCTYPE") || strings.Contains(body, "<html") {
		return summarizeHTMLPage(body, prefix)
	}
	if strings.HasSuffix(fmt.Sprintf("%T", err), "GeminiAPIError") {
		return redactSensitiveText(truncate(raw, 1000))
	}
	if payload := decodeJSONObject(body); payload != nil {
		var msg any
		if errObj, ok := payload["error"].(map[string]any); ok {
			msg = errObj["message"]
		} else {
			msg = payload["message"]
		}
		if msg != nil && msg != "" {
			return DecorateXAIEntitlementError(prefix + truncate(CoerceAPIErrorDetail(msg), 300))
		}
	}
	if snippet := strings.TrimSpace(body); snippet != "" {
		if payload := decodeJSONObject(snippet); payload != nil {
			if errObj, ok := payload["error"].(map[string]any); ok && errObj["message"] != nil && errObj["message"] != "" {
				return redactSensitiveText(prefix + truncate(fmt.Sprint(errObj["message"]), 300))
			}
			if m, ok := payload["message"]; ok && m != nil && m != "" {
				return redactSensitiveText(prefix + truncate(fmt.Sprint(m), 300))
			}
		}
		return redactSensitiveText(prefix + truncate(snippet, 300))
	}
	return DecorateXAIEntitlementError(prefix + truncate(raw, 500))
}

// MaskAPIKeyForLogs hides a key to its head and tail and names a bearer callable instead of invoking it
func MaskAPIKeyForLogs(key any) string {
	switch k := key.(type) {
	case nil:
		return ""
	case string:
		if k == "" {
			return ""
		}
		if len(k) <= 12 {
			return "***"
		}
		return k[:8] + "..." + k[len(k)-4:]
	}
	if reflect.TypeOf(key).Kind() == reflect.Func {
		return "<entra-id-bearer>"
	}
	return MaskAPIKeyForLogs(fmt.Sprint(key))
}

// CleanErrorMessage collapses whitespace, hides HTML pages, and caps display text at 150 chars
func CleanErrorMessage(errorMsg string) string {
	if errorMsg == "" {
		return "Unknown error"
	}
	if strings.HasPrefix(strings.TrimSpace(errorMsg), "<!DOCTYPE html") || strings.Contains(errorMsg, "<html") {
		return "Service temporarily unavailable (HTML error page returned)"
	}
	cleaned := strings.Join(strings.Fields(errorMsg), " ")
	if r := []rune(cleaned); len(r) > 150 {
		return string(r[:150]) + "..."
	}
	return cleaned
}
