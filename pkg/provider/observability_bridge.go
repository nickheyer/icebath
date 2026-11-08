package provider

import (
	"errors"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Notes a sign in failure's setup class on the open CLI provider flow
func noteSignInFailure(err error) { observability.NoteSignInFailure(err) }

// Marks the open CLI provider flow as having saved its provider and model choice
func noteProviderSetupSaved() { observability.NoteProviderSetupSaved() }

// Every FailoverReason the classifier returns, the model error class vocabulary
var failoverReasons = []FailoverReason{ReasonAuth, ReasonAuthPermanent, ReasonBilling, ReasonRateLimit, ReasonUpstreamRateLimit, ReasonUpstreamBlocked, ReasonOverloaded, ReasonServerError, ReasonTimeout, ReasonSSLCertVerification, ReasonContextOverflow, ReasonPayloadTooLarge, ReasonImageTooLarge, ReasonImageCorrupt, ReasonModelNotFound, ReasonProviderPolicyBlocked, ReasonDataPolicyDenied, ReasonContentPolicyBlocked, ReasonModelEntitlement, ReasonIncompleteResponse, ReasonFormatError, ReasonRoleAlternation, ReasonInvalidEncryptedContent, ReasonMultimodalToolContent, ReasonReasoningMandatory, ReasonThinkingSignature, ReasonLongContextTier, ReasonOAuthLongContextBetaForbid, ReasonLlamaCppGrammarPattern, ReasonCancelled, ReasonUnknown}

// FailoverReason enum construction by value, false for a value outside the enum
func ParseFailoverReason(value any) (FailoverReason, bool) {
	var raw string
	switch v := value.(type) {
	case FailoverReason:
		raw = string(v)
	case string:
		raw = v
	default:
		return "", false
	}
	for _, r := range failoverReasons {
		if string(r) == raw {
			return r, true
		}
	}
	return "", false
}

// Installs the provider hooks the shared metrics contract and setup classifier read
func init() {
	observability.FailoverReasonsHook = func() []string {
		out := make([]string, 0, len(failoverReasons))
		for _, r := range failoverReasons {
			out = append(out, string(r))
		}
		return out
	}
	observability.AuthErrorTraitsHook = &observability.AuthErrorTraits{
		IsCancelled:         IsCancelled,
		IsNetworkError:      IsNetworkError,
		IsDeviceCodeExpired: IsDeviceCodeExpired,
		IsAuthError: func(err error) bool {
			switch err.(type) {
			case *AuthError, *SignInCopyError:
				return true
			}
			return false
		},
		OAuthErrorCode: func(err error) string {
			var copyErr *SignInCopyError
			if errors.As(err, &copyErr) {
				return copyErr.OAuthErrorCode
			}
			return ""
		},
	}
	registerObservabilityProviderCatalogs()
}
