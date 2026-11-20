package provider

import (
	"strings"
)

// Kimi Code endpoint selected by an sk-kimi- prefixed key
const KimiCodeBaseURL = "https://api.kimi.com/coding"

// Cloud hosts a config base_url under provider auto never bypasses
var localBypassCloudHosts = []string{"openrouter.ai", "anthropic.com", "openai.com"}

// Provider auto with a config base_url at a non cloud host routes through the OpenAI compatible resolver
func (r *resolver) localEndpointBypass(requestedProvider, explicitAPIKey, explicitBaseURL string) *Runtime {
	modelCfg := r.modelConfig()
	cfgBaseURL := strings.TrimSpace(cfgString(modelCfg["base_url"]))
	if cfgBaseURL == "" {
		return nil
	}
	if p := cfgProviderOf(modelCfg); p != "auto" && p != "" {
		return nil
	}
	for _, host := range localBypassCloudHosts {
		if HostMatches(cfgBaseURL, host) {
			return nil
		}
	}
	return r.openrouterFallback(requestedProvider, explicitAPIKey, explicitBaseURL)
}

// Z.AI base URL, an explicit env override always winning and a keyless call never probing
func (r *resolver) resolveZaiBaseURL(apiKey, defaultURL, envOverride string) string {
	if envOverride != "" {
		return envOverride
	}
	if apiKey == "" {
		return defaultURL
	}
	return resolveZaiBaseURLDetected(r.ctx, r.home, apiKey, defaultURL)
}

// True when an AuthError is upstream rate limiting rather than a credential problem
func isRateLimitedAuthError(err error) bool {
	authErr, ok := err.(*AuthError)
	return ok && !authErr.ReloginRequired && authErr.Code == CodexRateLimitedCode
}

// IsRateLimitedAuthError reports an auth failure that is upstream rate limiting rather than a credential problem
func IsRateLimitedAuthError(err error) bool { return isRateLimitedAuthError(err) }
