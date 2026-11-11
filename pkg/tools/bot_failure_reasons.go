package tools

import (
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Platform side and agent side bot turn failure reason codes
const (
	ReasonRuntimeOffline        = "runtime_offline"
	ReasonQueuedExpired         = "queued_expired"
	ReasonDeliveryTimeout       = "delivery_timeout"
	ReasonAgentBlocked          = "agent_blocked"
	ReasonCancelled             = "cancelled"
	ReasonProviderAuthOrAccess  = "provider_auth_or_access"
	ReasonProviderQuotaLimit    = "provider_quota_limit"
	ReasonProviderRateLimit     = "provider_rate_limit"
	ReasonProviderServerError   = "provider_server_error"
	ReasonContextOverflow       = "context_overflow"
	ReasonMissingConfig         = "missing_config"
	ReasonModelUnavailable      = "model_unavailable"
	ReasonTargetScopeUnresolved = "target_scope_unresolved"
	ReasonUnknown               = "unknown"
)

// Closed vocabulary of bot turn failure reasons
var AllReasons = map[string]bool{
	ReasonRuntimeOffline: true, ReasonQueuedExpired: true, ReasonDeliveryTimeout: true, ReasonAgentBlocked: true,
	ReasonCancelled: true, ReasonProviderAuthOrAccess: true, ReasonProviderQuotaLimit: true,
	ReasonProviderRateLimit: true, ReasonProviderServerError: true, ReasonContextOverflow: true,
	ReasonMissingConfig: true, ReasonModelUnavailable: true, ReasonTargetScopeUnresolved: true, ReasonUnknown: true}

// Runtime refusal code a Bot Chat delivery id's settled not sent receipt carries
const HeldNotSent = "runtime_coordination_required"

// Reasons a supervisor may retry automatically without human intervention
var AutoRetryable = map[string]bool{ReasonRuntimeOffline: true, ReasonDeliveryTimeout: true,
	ReasonProviderRateLimit: true, ReasonProviderServerError: true}

// Whether reason may be retried without human intervention
func IsAutoRetryable(reason string) bool { return AutoRetryable[reason] }

// Bot turn retry actions
const (
	RetryResume             = "resume"
	RetryCompressThenResume = "compress_then_resume"
	RetryNone               = "none"
)

// Retry action of a failure reason, transient resuming, overflow compressing first, the rest never retried
func RetryAction(reason string) string {
	if AutoRetryable[reason] {
		return RetryResume
	}
	if reason == ReasonContextOverflow {
		return RetryCompressThenResume
	}
	return RetryNone
}

const failureStatus = `(?:error code:?\s*|status(?:\s*code)?:?\s*|http\s*)`

// Ordered failure text rules, the first match naming the reason
var failureRules = []struct {
	pattern *regexp.Regexp
	code    string
}{
	{regexp.MustCompile(`(?i)could not read this profile's [^(]+\(an internal profile-scoping bug|served_profile_child_env\(inherit_credentials=true\)`), ReasonTargetScopeUnresolved},
	{regexp.MustCompile(`(?i)authentication_error|invalid api key|` + failureStatus + `(?:401|403)\b`), ReasonProviderAuthOrAccess},
	{regexp.MustCompile(`(?i)` + failureStatus + `402\b|out of funds|quota|balance`), ReasonProviderQuotaLimit},
	{regexp.MustCompile(`(?i)` + failureStatus + `429\b|rate.?limit`), ReasonProviderRateLimit},
	{regexp.MustCompile(`(?i)` + failureStatus + `5\d{2}\b|server[ _]?error|overloaded`), ReasonProviderServerError},
	{regexp.MustCompile(`(?i)context length|context_overflow|maximum context`), ReasonContextOverflow},
	{regexp.MustCompile(`(?i)no llm provider configured|missing config|no access token`), ReasonMissingConfig},
	{regexp.MustCompile(`(?i)model .*(not found|does not exist)|model_not_found`), ReasonModelUnavailable},
}

// Failure text of a delivery turn, both non blank streams stripped and joined in the order written
func TurnFailureText(stdout, stderr any) (string, error) {
	var parts []string
	for _, value := range []any{stdout, stderr} {
		if !pyrepr.Truthy(value) {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return "", pyAttributeError(value, "strip")
		}
		if stripped := pystr.Strip(text); stripped != "" {
			parts = append(parts, stripped)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// Retry action of a finished in process turn result, none for anything that did not fail
func ResultRetryAction(result any) (string, error) {
	fields, ok := turnResultFields(result)
	if !ok || !pyrepr.Truthy(fields["failed"]) {
		return RetryNone, nil
	}
	text, err := TurnFailureText(fields["error"], fields["failure_reason"])
	if err != nil {
		return "", err
	}
	return RetryAction(ClassifyAgentError(text)), nil
}

// Mapping view of a turn result, false for anything that is no dict
func turnResultFields(result any) (map[string]any, bool) {
	switch r := result.(type) {
	case map[string]any:
		return r, r != nil
	case interface {
		Get(string) (any, bool)
		Len() int
	}:
		if r == nil {
			return nil, false
		}
		fields := map[string]any{}
		for _, key := range []string{"failed", "error", "failure_reason"} {
			if value, present := r.Get(key); present {
				fields[key] = value
			}
		}
		return fields, true
	}
	return nil, false
}

// Closed reason code of raw agent or provider error text, unknown when unmatched or empty
func ClassifyAgentError(text any) string {
	raw := ""
	if pyrepr.Truthy(text) {
		raw = pyrepr.Str(text)
	}
	if pystr.Strip(raw) != "" {
		for _, rule := range failureRules {
			if rule.pattern.MatchString(raw) {
				return rule.code
			}
		}
	}
	return ReasonUnknown
}

// Error that carries its own reason attribute
type ReasonedError interface {
	error
	ReasonAttr() string
}

// Typed reason of a delivery refusal, an error's own reason trusted only inside the vocabulary
func DeliveryFailureReason(err error) string {
	supplied := ""
	switch e := err.(type) {
	case *state.RuntimeStoreError:
		supplied = e.Reason
	case ReasonedError:
		supplied = e.ReasonAttr()
	}
	supplied = pystr.Strip(supplied)
	if supplied == "target_busy" || AllReasons[supplied] {
		return supplied
	}
	return ClassifyAgentError(err.Error())
}
