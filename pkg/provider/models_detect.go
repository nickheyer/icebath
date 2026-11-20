package provider

import (
	"context"
	"strings"
)

// Providers whose own OpenRouter step answers the ladder or that have no catalog to consult
var detectSkipProviders = map[string]bool{"": true, "auto": true, "openrouter": true, "custom": true}

// The current provider's own spelling of a model its cached live catalog serves, empty otherwise
func currentProviderCatalogMatch(ctx context.Context, modelName, currentProvider string) string {
	provider := strings.ToLower(strings.TrimSpace(currentProvider))
	if detectSkipProviders[provider] || strings.HasPrefix(provider, "custom:") || detectSkipProviders[catalogNormalizeProvider(provider)] {
		return ""
	}
	wanted := strings.ToLower(strings.TrimSpace(modelName))
	if wanted == "" {
		return ""
	}
	catalog := cachedProviderModelIDsFn(ctx, provider, CachedModelsOptions{})
	for _, mid := range catalog {
		if strings.ToLower(mid) == wanted {
			return mid
		}
	}
	for _, mid := range catalog {
		if _, bare, ok := strings.Cut(mid, "/"); ok && strings.ToLower(bare) == wanted {
			return mid
		}
	}
	return ""
}

// True when the model belongs to the single vendor a first party provider natively serves
func currentProviderOwnsVendor(ctx context.Context, modelName, currentProvider string) bool {
	provider := strings.ToLower(strings.TrimSpace(currentProvider))
	if detectSkipProviders[provider] || strings.HasPrefix(provider, "custom:") {
		return false
	}
	normalized := catalogNormalizeProvider(provider)
	if detectSkipProviders[normalized] || IsAggregatorProvider(normalized) {
		return false
	}
	vendor := DetectVendor(modelName)
	if vendor == "" {
		return false
	}
	native := map[string]bool{}
	for _, mid := range staticCatalogModels(normalized) {
		native[DetectVendor(mid)] = true
	}
	return len(native) == 1 && native[vendor]
}

// ProviderHasCredentials reports whether a provider can be switched to without typing a key
func ProviderHasCredentials(ctx context.Context, provider string) bool {
	pid := strings.ToLower(strings.TrimSpace(provider))
	if pid == "" {
		return false
	}
	if pid == "custom" || strings.HasPrefix(pid, "custom:") {
		return true
	}
	env := modelsPoolEnv(ctx)
	if pid == "openrouter" && hasUsableSecret(GetEnvPreferDotenv(env, "OPENROUTER_API_KEY")) {
		return true
	}
	loggedIn, configured := providerAuthStatusFn(ctx, pid)
	if loggedIn || configured {
		return true
	}
	pool := LoadPool(env, pid)
	return pool != nil && pool.HasCredentials() && pool.HasAvailable("")
}
