package provider

import (
	"context"
	"strings"
)

// FailureScope names which identity axis a failure invalidates
type FailureScope string

const (
	// Timeout, overload, connection blip, invalid response, evidence against one model deployment
	ScopeModel FailureScope = "model"
	// Auth 401 or payment 402, evidence against the shared credential
	ScopeCredential FailureScope = "credential"
	// DNS, connection refused, unreachable host, evidence against the endpoint
	ScopeEndpoint FailureScope = "endpoint"
)

func normIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// BackendIdentity is the normalized provider, model, and endpoint of one deployment with empty meaning unknown
type BackendIdentity struct {
	Provider string
	Model    string
	BaseURL  string
}

// BuildBackendIdentity normalizes the three axes of a candidate route
func BuildBackendIdentity(provider, model, baseURL string) BackendIdentity {
	return BackendIdentity{Provider: normIdentity(provider), Model: normIdentity(model), BaseURL: NormalizeRouteBaseURL(baseURL)}
}

// Raw membership of a provider id in the auth registry of the context home
func providerInRegistry(ctx context.Context, provider string) bool {
	return providerRegistry().contains(ctx, provider)
}

// True when both providers are distinct rows of the auth registry the context home sees
func bothFirstClassIn(ctx context.Context, a, b BackendIdentity) bool {
	if a.Provider == "" || b.Provider == "" || a.Provider == b.Provider {
		return false
	}
	return providerInRegistry(ctx, a.Provider) && providerInRegistry(ctx, b.Provider)
}

// SameCredentialSurface reports two identities sharing the credential a 401 or 402 just invalidated
func SameCredentialSurface(a, b BackendIdentity) bool {
	if a.Provider != "" && b.Provider != "" {
		return a.Provider == b.Provider
	}
	return a.BaseURL != "" && a.BaseURL == b.BaseURL
}

// SameEndpoint reports two identities behind the endpoint that just went unreachable
func SameEndpoint(a, b BackendIdentity) bool {
	if a.BaseURL != "" && b.BaseURL != "" {
		return a.BaseURL == b.BaseURL
	}
	return a.Provider != "" && a.Provider == b.Provider
}

// SameDeploymentIn reports the exact same model deployment with first class providers read from the context home
func SameDeploymentIn(ctx context.Context, a, b BackendIdentity) bool {
	return sameDeployment(a, b, func(a, b BackendIdentity) bool { return bothFirstClassIn(ctx, a, b) })
}

// Provider and model match, a shared url and model across labels counting unless both labels are first class
func sameDeployment(a, b BackendIdentity, firstClass func(a, b BackendIdentity) bool) bool {
	if !(a.Provider != "" && b.Provider != "" && a.Provider == b.Provider) {
		return a.BaseURL != "" && a.BaseURL == b.BaseURL && a.Model != "" && a.Model == b.Model && !firstClass(a, b)
	}
	if !(a.Model != "" && b.Model != "" && a.Model == b.Model) {
		return false
	}
	return !(a.BaseURL != "" && b.BaseURL != "" && a.BaseURL != b.BaseURL)
}

// ShouldSkipCandidateIn is the skip test with first class providers read from the context home
func ShouldSkipCandidateIn(ctx context.Context, candidate, failed BackendIdentity, scope FailureScope) bool {
	return shouldSkipCandidate(candidate, failed, scope, func(a, b BackendIdentity) bool { return SameDeploymentIn(ctx, a, b) })
}

// Sameness along the axis the failure invalidated, the deployment test for every other scope
func shouldSkipCandidate(candidate, failed BackendIdentity, scope FailureScope, sameDeployment func(a, b BackendIdentity) bool) bool {
	switch scope {
	case ScopeCredential:
		return SameCredentialSurface(candidate, failed)
	case ScopeEndpoint:
		return SameEndpoint(candidate, failed)
	}
	return sameDeployment(candidate, failed)
}

var fallbackReasonLabels = map[FailoverReason]string{
	ReasonAuth: "authentication failed", ReasonAuthPermanent: "authentication permanently failed",
	ReasonBilling: "billing or quota exhausted", ReasonRateLimit: "rate limit",
	ReasonUpstreamRateLimit: "upstream model rate limit", ReasonOverloaded: "provider overloaded",
	ReasonServerError: "provider server error", ReasonTimeout: "request timeout",
	ReasonSSLCertVerification: "TLS certificate verification failed", ReasonContextOverflow: "context window exceeded",
	ReasonPayloadTooLarge: "request payload too large", ReasonImageTooLarge: "image payload too large",
	ReasonModelNotFound: "model not found", ReasonProviderPolicyBlocked: "provider policy blocked the request",
	ReasonContentPolicyBlocked: "content policy blocked the request", ReasonFormatError: "request format rejected",
	ReasonRoleAlternation: "adjacent same-role messages rejected", ReasonInvalidEncryptedContent: "encrypted reasoning state rejected",
	ReasonMultimodalToolContent: "multimodal tool content unsupported", ReasonThinkingSignature: "thinking signature rejected",
	ReasonLongContextTier: "long-context tier unavailable", ReasonOAuthLongContextBetaForbid: "OAuth long-context beta unavailable",
	ReasonLlamaCppGrammarPattern: "grammar pattern rejected", ReasonUnknown: "provider failure",
}

// FallbackReasonText is the concise operator facing explanation for a fallback switch
func FallbackReasonText(reason FailoverReason) string {
	if label, ok := fallbackReasonLabels[reason]; ok {
		return label
	}
	if reason == "" {
		return "provider failure"
	}
	return strings.ReplaceAll(string(reason), "_", " ")
}
