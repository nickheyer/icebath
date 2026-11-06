package provider

import "context"

// PromptCachePlanOptions selects the cache layout of one resolved request destination
type PromptCachePlanOptions struct {
	TTL                   string
	NativeAnthropic       bool
	StaticSystemPrefix    string
	DirectNativeToolCache bool
	ToolPartMarkers       bool
}

// BuildPromptCachePlan applies the copy on write cache sections to the request messages and tools
func BuildPromptCachePlan(apiMessages, tools []map[string]any, o PromptCachePlanOptions) ([]map[string]any, []map[string]any) {
	return buildPromptCachePlan(apiMessages, tools, cachePlanOptions{
		ttl: o.TTL, nativeAnthropic: o.NativeAnthropic, staticSystemPrefix: o.StaticSystemPrefix,
		directNativeToolCache: o.DirectNativeToolCache, toolPartMarkers: o.ToolPartMarkers,
	})
}

// EffectiveCacheTTL resolves the cache lifetime the route honours for the configured TTL
func EffectiveCacheTTL(ttl, model, providerName string) string {
	return effectiveCacheTTL(ttl, model, providerName)
}

// StripAnthropicCacheControl removes cache control markers from the request messages
func StripAnthropicCacheControl(messages []map[string]any) []map[string]any {
	return stripAnthropicCacheControl(messages)
}

// StripAnthropicToolCacheControl removes cache control markers from the tool schemas
func StripAnthropicToolCacheControl(tools []map[string]any) []map[string]any {
	return stripAnthropicToolCacheControl(tools)
}

// EnvelopeToolPartCacheMarkersSupported reports whether the route accepts cache markers on tool parts
func EnvelopeToolPartCacheMarkersSupported(providerName, baseURL string) bool {
	return envelopeToolPartCacheMarkersSupported(providerName, baseURL)
}

// RefreshRouteProfiles returns the profiles overriding the runtime credential refresh for the route
func RefreshRouteProfiles(ctx context.Context, providerName, baseURL string) []*Profile {
	return refreshRouteProfiles(ctx, providerName, baseURL)
}

// WindowHardStop returns the usage window stop line when the window blocks the request
func WindowHardStop(state *CreditsState, modelIsFree bool, modelIsTopTier *bool) (string, bool) {
	return windowHardStop(state, modelIsFree, modelIsTopTier)
}
