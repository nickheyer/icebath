package provider

import "context"

// Whether a provider name currently resolves to a routable route under the context home config
func IsRoutableProvider(ctx context.Context, provider string) bool {
	return modelsResolver(ctx).IsRoutableProvider(provider)
}

// Inline or env backed key of a fallback entry through the secret scope aware resolver
func ResolveEntryAPIKey(ctx context.Context, entry map[string]any) string {
	return auxFallbackEntryAPIKey(ctx, entry)
}
