// Routes model calls and classifies their failures with retry waits, error summaries and token estimates
package model

import (
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/portal"
)

type Class uint8

const (
	ClassUnknown Class = iota
	ClassAuth
	ClassAuthPermanent
	ClassBilling
	ClassRateLimit
	ClassUpstreamRateLimit
	ClassUpstreamBlocked
	ClassOverloaded
	ClassServerError
	ClassTimeout
	ClassSSLCertVerification
	ClassContextOverflow
	ClassPayloadTooLarge
	ClassImageTooLarge
	ClassImageCorrupt
	ClassModelNotFound
	ClassProviderPolicyBlocked
	ClassDataPolicyDenied
	ClassContentPolicyBlocked
	ClassModelEntitlement
	ClassIncompleteResponse
	ClassFormatError
	ClassRoleAlternation
	ClassInvalidEncryptedContent
	ClassMultimodalToolContent
	ClassReasoningMandatory
	ClassThinkingSignature
	ClassLongContextTier
	ClassOAuthLongContextBeta
	ClassGrammarPattern
)

var classNames = [...]string{
	ClassUnknown:                 "unknown",
	ClassAuth:                    "auth",
	ClassAuthPermanent:           "auth_permanent",
	ClassBilling:                 "billing",
	ClassRateLimit:               "rate_limit",
	ClassUpstreamRateLimit:       "upstream_rate_limit",
	ClassUpstreamBlocked:         "upstream_blocked",
	ClassOverloaded:              "overloaded",
	ClassServerError:             "server_error",
	ClassTimeout:                 "timeout",
	ClassSSLCertVerification:     "ssl_cert_verification",
	ClassContextOverflow:         "context_overflow",
	ClassPayloadTooLarge:         "payload_too_large",
	ClassImageTooLarge:           "image_too_large",
	ClassImageCorrupt:            "image_corrupt",
	ClassModelNotFound:           "model_not_found",
	ClassProviderPolicyBlocked:   "provider_policy_blocked",
	ClassDataPolicyDenied:        "data_policy_denied",
	ClassContentPolicyBlocked:    "content_policy_blocked",
	ClassModelEntitlement:        "model_entitlement",
	ClassIncompleteResponse:      "incomplete_response",
	ClassFormatError:             "format_error",
	ClassRoleAlternation:         "role_alternation",
	ClassInvalidEncryptedContent: "invalid_encrypted_content",
	ClassMultimodalToolContent:   "multimodal_tool_content_unsupported",
	ClassReasoningMandatory:      "reasoning_mandatory",
	ClassThinkingSignature:       "thinking_signature",
	ClassLongContextTier:         "long_context_tier",
	ClassOAuthLongContextBeta:    "oauth_long_context_beta_forbidden",
	ClassGrammarPattern:          "llama_cpp_grammar_pattern",
}

func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "class(" + strconv.Itoa(int(c)) + ")"
}

func ParseClass(name string) (Class, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for i, n := range classNames {
		if n == name {
			return Class(i), true
		}
	}
	return ClassUnknown, false
}

func RetryableClient(c Class) bool {
	switch c {
	case ClassRateLimit, ClassUpstreamRateLimit, ClassOverloaded, ClassContextOverflow,
		ClassPayloadTooLarge, ClassLongContextTier, ClassThinkingSignature:
		return true
	}
	return false
}

type Hints struct {
	Retryable        bool
	Compress         bool
	RotateCredential bool
	Fallback         bool
}

type Detail struct {
	ResetAt               time.Time
	BillingUnverified     bool
	PossibleContentFilter bool
	UpstreamProvider      string
	WelcomeRefusal        *portal.Refusal
	WelcomeRoute          string
	UsageWindow           bool
	DisplayMessage        string
}

type Error struct {
	Class Class
	Hints
	Status          int
	Provider, Model string
	Message         string
	Detail
	Err error
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Class.String()
	}
	return e.Class.String() + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) IsAuth() bool { return e.Class == ClassAuth || e.Class == ClassAuthPermanent }

type StaleCallError struct {
	Attempts int
}

func (e *StaleCallError) Error() string {
	return "Provider has been unresponsive (no response received) for " + strconv.Itoa(e.Attempts) +
		" consecutive stale attempts — aborting this call to avoid an indefinite stall. " +
		"Switch models or start a new session, then retry."
}

type StreamLifetimeError struct {
	Elapsed time.Duration
	Cap     time.Duration
}

func (e *StreamLifetimeError) Error() string {
	return "Response still streaming after " + strconv.FormatFloat(e.Elapsed.Seconds(), 'f', 0, 64) +
		"s, cut by agent.stream_max_lifetime (" + strconv.FormatFloat(e.Cap.Seconds(), 'g', -1, 64) + "s). " +
		"Raise agent.stream_max_lifetime, or set it to 0 to turn the cap off, if this model legitimately streams this long."
}

type PresetNotFoundError struct {
	Preset string
}

func (e *PresetNotFoundError) Error() string {
	return "mixture-of-agents preset " + strconv.Quote(e.Preset) + " no longer exists in config"
}
