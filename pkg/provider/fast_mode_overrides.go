package provider

import "strings"

// OpenAI flagships eligible for Priority Processing
var fastModeOpenAIPrefixes = []string{"gpt-", "o1", "o3", "o4"}

// Exact wire slugs OpenAI publishes Ultrafast for
var fastModeUltrafastModels = map[string]bool{"gpt-6-astra": true, "gpt-6.1-sol": true}

// Lowercases and strips a vendor prefix
func stripVendorPrefixLower(model string) string {
	raw := strings.ToLower(strings.TrimSpace(model))
	if i := strings.Index(raw, "/"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

func isOpenAIFastModel(model string) bool {
	base := strings.SplitN(stripVendorPrefixLower(model), ":", 2)[0]
	if base == "" || strings.Contains(base, "codex") {
		return false
	}
	for _, p := range fastModeOpenAIPrefixes {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// ModelSupportsFastMode reports whether the fast toggle is exposed for the model
func ModelSupportsFastMode(model string) bool {
	return IsAnthropicFastModeModel(model) || isOpenAIFastModel(model) || IsGrok46Family(model)
}

// Only the first party endpoint that bills for fast mode may receive its params
func fastModeRouteSupported(model, providerName, baseURL string) bool {
	var allowed map[string]string
	switch {
	case IsAnthropicFastModeModel(model):
		allowed = map[string]string{"anthropic": "api.anthropic.com"}
	case IsGrok46Family(model):
		allowed = map[string]string{"xai": "api.x.ai"}
	default:
		allowed = map[string]string{"openai": "api.openai.com", "openai-api": "api.openai.com", "openai-codex": "chatgpt.com"}
	}
	if providerName != "" {
		if _, ok := allowed[authNormalizeProvider(providerName)]; !ok {
			return false
		}
	}
	host := Hostname(baseURL)
	if host == "" {
		return true
	}
	for _, h := range allowed {
		if h == host {
			return true
		}
	}
	return false
}

// ModelSupportsUltrafast reports whether OpenAI publishes the ultrafast tier for the exact slug
func ModelSupportsUltrafast(model string) bool {
	base := strings.SplitN(stripVendorPrefixLower(StripCodexContextVariantSuffix(model)), ":", 2)[0]
	return fastModeUltrafastModels[base]
}

// ResolveFastModeOverrides returns the fast or priority request overrides, nil when unsupported
func ResolveFastModeOverrides(model, providerName, baseURL, tier string) map[string]any {
	if !ModelSupportsFastMode(model) {
		return nil
	}
	if (providerName != "" || baseURL != "") && !fastModeRouteSupported(model, providerName, baseURL) {
		return nil
	}
	if tier == "ultrafast" {
		if ModelSupportsUltrafast(model) {
			return map[string]any{"service_tier": "ultrafast"}
		}
		return nil
	}
	if IsAnthropicFastModeModel(model) {
		return map[string]any{"speed": "fast"}
	}
	return map[string]any{"service_tier": "priority"}
}
