package provider

import (
	"context"
	"os"
	"strings"
)

// Stripped text of an optional provider field
func identityClean(v string) string { return strings.TrimSpace(v) }

// Identity of a session route, the requested entry while it still serves base URL, else the endpoint owner, else the adapter
func SessionProviderIdentity(ctx context.Context, provider, requestedProvider, baseURL, model string, cfg map[string]any) string {
	adapter := identityClean(provider)
	if strings.ToLower(adapter) != "custom" {
		return adapter
	}
	requested := identityClean(requestedProvider)
	url := routeKey(baseURL)
	r := modelsResolver(ctx)
	if url != "" {
		switch strings.ToLower(requested) {
		case "", "custom", "auto":
		default:
			if entry := r.getNamedCustomProvider(requested, cfg, false); entry != nil && routeKey(entryURL(entry)) == url {
				return requested
			}
		}
		if identity := r.endpointIdentity(identityClean(baseURL)); identity != "" {
			return identity
		}
		return adapter
	}
	if identity := CanonicalCustomIdentity(ctx, "", "", identityClean(model), ""); identity != "" {
		return identity
	}
	return adapter
}

// Durable menu identity of a bare custom provider from its endpoint, model or configured provider, empty when none heals it
func CanonicalCustomIdentity(ctx context.Context, baseURL, configProvider, model, requested string) string {
	identity := "custom"
	if baseURL != "" {
		identity = SessionProviderIdentity(ctx, "custom", requested, baseURL, "", nil)
	}
	if identity != "custom" {
		return identity
	}
	r := modelsResolver(ctx)
	if model != "" {
		if identity := FindCustomProviderIdentityByModel(model, r.cfg); identity != "" {
			return identity
		}
	}
	candidate := identityClean(configProvider)
	if candidate == "" {
		if modelCfg, ok := r.cfg["model"].(map[string]any); ok {
			if p, ok := modelCfg["provider"].(string); ok {
				candidate = identityClean(p)
			}
		}
	}
	if candidate == "" {
		candidate = identityClean(os.Getenv("ICEBATH_INFERENCE_PROVIDER"))
	}
	candidateNorm := normalizeCustomProviderName(candidate)
	switch candidateNorm {
	case "", "custom", "auto", "openrouter":
		return ""
	}
	entry := r.getNamedCustomProvider(candidate, nil, false)
	if entry == nil {
		return ""
	}
	baseURLValue, _ := entry["base_url"].(string)
	if identity := FindCustomProviderIdentity(baseURLValue, r.cfg); identity != "" {
		return identity
	}
	return CustomProviderSlug(candidateNorm, "")
}
