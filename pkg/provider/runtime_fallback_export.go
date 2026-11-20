package provider

import "context"

// Primary runtime of the home, the configured fallback chain walked when the primary fails on credentials
func ResolveRuntimeWithFallback(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*Runtime, map[string]any, error) {
	return modelsResolver(ctxForHome(ctx, h)).ResolveRuntimeWithFallback(requested, targetModel, explicitBaseURL, explicitAPIKey)
}

// Model section of the home's config as the runtime resolver reads it
func PrimaryModelConfig(ctx context.Context, h string) map[string]any {
	return modelsResolver(ctxForHome(ctx, h)).modelConfig()
}

// Named custom rung of the runtime resolver over an explicit config, nil when the request is no custom route
func ResolveNamedCustomRuntime(ctx context.Context, h, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel string, cfg map[string]any) (*Runtime, error) {
	return modelsResolver(ctxForHome(ctx, h)).resolveNamedCustomRuntime(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel, cfg)
}
