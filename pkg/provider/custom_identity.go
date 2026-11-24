package provider

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Stripped text of an optional provider field
func identityClean(v string) string { return strings.TrimSpace(v) }

// Identity of a session route under the settings ctx gives, the requested entry while it still serves base URL, else the endpoint owner, else the adapter
func SessionProviderIdentity(ctx context.Context, provider, requestedProvider, baseURL, model string) string {
	adapter := identityClean(provider)
	if strings.ToLower(adapter) != "custom" {
		return adapter
	}
	requested := identityClean(requestedProvider)
	url := routeKey(baseURL)
	r := modelsResolver(ctx)
	providers := r.providers()
	if url != "" {
		switch strings.ToLower(requested) {
		case "", "custom", "auto":
		default:
			if entry := r.getNamedCustomProvider(requested, providers, false); entry != nil && routeKey(entryURL(entry)) == url {
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
		identity = SessionProviderIdentity(ctx, "custom", requested, baseURL, "")
	}
	if identity != "custom" {
		return identity
	}
	r := modelsResolver(ctx)
	providers := r.providers()
	if model != "" {
		if identity := FindCustomProviderIdentityByModel(model, providers); identity != "" {
			return identity
		}
	}
	candidate := identityClean(configProvider)
	if candidate == "" {
		candidate = identityClean(settings.ModelProvider.Get(r.hookCtx()))
	}
	if candidate == "" {
		candidate = identityClean(os.Getenv("ICEBATH_INFERENCE_PROVIDER"))
	}
	candidateNorm := normalizeCustomProviderName(candidate)
	switch candidateNorm {
	case "", "custom", "auto", "openrouter":
		return ""
	}
	entry := r.getNamedCustomProvider(candidate, providers, false)
	if entry == nil {
		return ""
	}
	baseURLValue, _ := entry["base_url"].(string)
	if identity := FindCustomProviderIdentity(baseURLValue, providers); identity != "" {
		return identity
	}
	return CustomProviderSlug(candidateNorm, "")
}
