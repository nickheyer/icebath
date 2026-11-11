package provider

import (
	"context"
	"time"
)

// Declared model ids of a provider entry's models config in declaration order
func DeclaredModelIDs(value any) []string { return declaredModelIDs(value) }

// Reports whether a provider entry marks its models as discovered
func EntryModelsDiscovered(entry any) bool { return entryModelsDiscovered(entry) }

// Reports whether a models config acts as an allowlist over discovered models
func ModelsConfigIsAllowlist(value any, discovered bool) bool {
	return modelsConfigIsAllowlist(value, discovered)
}

// Reports whether live model discovery is enabled for a provider entry
func DiscoverFlag(entry map[string]any) bool { return discoverFlag(entry) }

// Cached picker fetch of an endpoint's live models, Native set for an Ollama native catalog, false when unavailable
func FetchPickerLiveModels(ctx context.Context, apiKey any, apiURL, nativeProvider string, preserveNative bool, headers map[string]string, timeout time.Duration, apiMode string) (CatalogRows, bool) {
	return fetchPickerLiveModels(ctx, apiKey, apiURL, nativeProvider, preserveNative, headers, timeout, apiMode, true)
}

// Configured custom provider ids of the active config
func ConfiguredCustomProviderIDs(ctx context.Context) map[string]bool {
	return configuredCustomProviderIDs(ctx)
}
