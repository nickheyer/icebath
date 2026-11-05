package provider

import (
	"context"
	"strings"
)

// Endpoint the main runtime would use, model.base_url for this provider or the registered profile's own
func auxConfiguredEndpoint(ctx context.Context, provider string) string {
	if u := configBaseURLForProviderCtx(ctx, getModelConfigDict(ctx), provider); u != "" {
		return u
	}
	if p, ok := registryProfileCtx(ctx, provider); ok {
		return p.BaseURL
	}
	return ""
}

// Client and model for a plugin OAuth provider, empty when signed out or transport less
func auxResolvePluginOAuthClient(ctx context.Context, req auxResolveRequest) auxResolveResult {
	provider := req.Provider
	apiKey := auxNormalizeAPIKey(req.ExplicitAPIKey)
	var entry *PooledCredential
	if !auxTruthyKey(apiKey) {
		_, entry = auxSelectPoolEntry(ctx, provider)
		apiKey = ""
		if entry != nil {
			apiKey = entry.RuntimeAPIKey()
		}
	}
	baseURL := req.ExplicitBaseURL
	if baseURL == "" && entry != nil {
		baseURL = entry.RuntimeBaseURL()
	}
	if baseURL == "" {
		baseURL = auxConfiguredEndpoint(ctx, provider)
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	var client AuxClient
	if auxTruthyKey(apiKey) {
		client = auxAPIKeyProfileSuppliedClient(ctx, provider, apiKey, baseURL)
	}
	if client == nil {
		auxLogOnceDebug(auxLoggedUnsupportedOAuthKeys, provider,
			"resolve_provider_client: OAuth provider %s has no signed-in credential or plugin transport, try 'auto'", provider)
		return auxResolveResult{}
	}
	model := req.Model
	if model == "" {
		model = auxGetAuxModelForProvider(ctx, provider)
	}
	return auxRouteClient(ctx, req, client, auxNormalizeResolvedModel(model, provider))
}

// Python truthiness of a normalized key, a non empty string or a key provider
func auxTruthyKey(key any) bool {
	switch k := key.(type) {
	case nil:
		return false
	case string:
		return k != ""
	case func() (string, error):
		return k != nil
	}
	return true
}
