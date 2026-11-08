package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Current .env key, base url and registry default of an api key or named custom provider, ok false when none
func ResolveEnvCredentials(env PoolEnv, providerID, requestedProvider string) (apiKey, baseURL, defaultBase string, ok bool) {
	r := authResolver(env)
	pconfig, found := registryProfileCtx(env.ctx(), providerID)
	switch {
	case found && pconfig.AuthType == AuthTypeAPIKey && len(pconfig.KeyEnv) > 0:
		for _, name := range pconfig.KeyEnv {
			if key := strings.TrimSpace(GetEnvPreferDotenv(env, name)); key != "" {
				apiKey = key
				break
			}
		}
		if apiKey == "" {
			return "", "", "", false
		}
		envURL := ""
		if pconfig.BaseURLEnv != "" {
			envURL = strings.TrimRight(strings.TrimSpace(GetEnvPreferDotenv(env, pconfig.BaseURLEnv)), "/")
		}
		defaultBase = strings.TrimRight(strings.TrimSpace(pconfig.BaseURL), "/")
		baseURL = envURL
		if baseURL == "" {
			baseURL = defaultBase
		}
		switch providerID {
		case "actual":
			configured := configBaseURLForProviderCtx(env.ctx(), r.modelConfig(), "actual")
			if configured == "" {
				configured = baseURL
			}
			baseURL = NormalizeActualBaseURL(configured)
		case "kimi-coding":
			baseURL = strings.TrimRight(resolveKimiBaseURL(apiKey, pconfig.BaseURL, envURL), "/")
		case "zai":
			baseURL = strings.TrimRight(r.resolveZaiBaseURL(apiKey, pconfig.BaseURL, envURL), "/")
		}
	case providerID == "custom":
		entry := r.getNamedCustomProvider(requestedProvider, nil, true)
		keyEnv := ""
		if v := entry["key_env"]; pyrepr.Truthy(v) {
			keyEnv = strings.TrimSpace(pyStr(v))
		}
		if keyEnv != "" {
			apiKey = strings.TrimSpace(GetEnvPreferDotenv(env, keyEnv))
		}
		if entry == nil || apiKey == "" {
			return "", "", "", false
		}
		if v := entry["base_url"]; pyrepr.Truthy(v) {
			baseURL = strings.TrimRight(strings.TrimSpace(pyStr(v)), "/")
		}
		defaultBase = baseURL
	default:
		return "", "", "", false
	}
	if baseURL == "" {
		return "", "", "", false
	}
	return apiKey, baseURL, defaultBase, true
}
