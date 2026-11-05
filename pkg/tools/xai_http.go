package tools

import (
	"net/url"
	"strings"
)

// DefaultXAIBaseURL is the xAI REST origin for API key callers
const DefaultXAIBaseURL = "https://api.x.ai/v1"

// Inference origin of the SuperGrok OAuth grant
const defaultXAIOAuthBaseURL = "https://api.x.ai/v1"

// CredentialPoolSource selects credential pool entries, installed by the provider wiring
type CredentialPoolSource interface {
	// Runtime bearer and base URL of the selected entry, expiring rows refreshed, ok false when none is usable
	SelectPooled(env *Env, provider string) (token, baseURL string, ok bool)
}

// CredentialPools is the installed pool source, nil when no pool backend is wired
var CredentialPools CredentialPoolSource

// Bearer credentials for one direct xAI HTTP call
type xaiHTTPCredentials struct {
	Provider string
	APIKey   string
	BaseURL  string
}

// Stable icebath user agent for xAI HTTP calls
func icebathXAIUserAgent() string {
	return "Icebath-Agent/" + icebathVersion()
}

// Reports whether a host is the x.ai apex or one of its subdomains
func isXAIOriginHost(host string) bool {
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

// Problem with an xAI URL, scheme, host, origin, or empty when it is HTTPS on x.ai
func xaiURLProblem(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme != "https":
		return "scheme", host, nil
	case host == "":
		return "host", host, nil
	case !isXAIOriginHost(host):
		return "origin", host, nil
	}
	return "", host, nil
}

// Pins an xAI base URL override to the xAI origin, warning and using the fallback on rejection
func xaiValidateInferenceBaseURL(env *Env, value, fallback string) string {
	candidate := strings.TrimRight(strings.TrimSpace(value), "/")
	if candidate == "" {
		return fallback
	}
	log := envLog(env)
	problem, host, err := xaiURLProblem(candidate)
	if err != nil {
		log.Warn("Ignoring malformed xAI base_url override; using the fallback instead.", "override", candidate, "fallback", fallback)
		return fallback
	}
	switch problem {
	case "":
		return candidate
	case "scheme":
		log.Warn("Refusing non-HTTPS xAI base_url override (xai-oauth bearer would be sent in cleartext); falling back.", "override", candidate, "fallback", fallback)
	case "host":
		log.Warn("Ignoring xAI base_url override with no hostname; using the fallback instead.", "override", candidate, "fallback", fallback)
	default:
		log.Warn("Refusing xAI base_url override, host is not on the xAI origin (expected x.ai or a *.x.ai subdomain); the bearer would leak.",
			"override", candidate, "host", host, "fallback", fallback)
	}
	return fallback
}

// xAI base URL override from ICEBATH_XAI_BASE_URL then XAI_BASE_URL, empty when unset
func xaiBaseURLOverride(env *Env) string {
	v := secretValue(env, "ICEBATH_XAI_BASE_URL")
	if v == "" {
		v = secretValue(env, "XAI_BASE_URL")
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// XAI_API_KEY through config, profile scope, .env, then the credential pool
func resolveExplicitXAIAPIKey(env *Env) string {
	return ResolveProviderSecret(env, "XAI_API_KEY", "xai", "", nil)
}

// Bearer credentials for direct xAI endpoints, OAuth first unless an explicit API key is preferred
func resolveXAIHTTPCredentials(env *Env, preferAPIKey bool) xaiHTTPCredentials {
	if preferAPIKey {
		if explicit := strings.TrimSpace(resolveExplicitXAIAPIKey(env)); explicit != "" {
			return xaiHTTPCredentials{Provider: "xai", APIKey: explicit,
				BaseURL: xaiValidateInferenceBaseURL(env, xaiBaseURLOverride(env), DefaultXAIBaseURL)}
		}
	}
	if CredentialPools != nil {
		if token, base, ok := CredentialPools.SelectPooled(env, "xai-oauth"); ok && strings.TrimSpace(token) != "" {
			fallback := strings.TrimRight(strings.TrimSpace(base), "/")
			if fallback == "" {
				fallback = defaultXAIOAuthBaseURL
			}
			return xaiHTTPCredentials{Provider: "xai-oauth", APIKey: strings.TrimSpace(token),
				BaseURL: xaiValidateInferenceBaseURL(env, xaiBaseURLOverride(env), fallback)}
		}
	}
	base := strings.TrimRight(strings.TrimSpace(secretValue(env, "XAI_BASE_URL")), "/")
	if base == "" {
		base = DefaultXAIBaseURL
	}
	return xaiHTTPCredentials{Provider: "xai", APIKey: resolveExplicitXAIAPIKey(env), BaseURL: base}
}
