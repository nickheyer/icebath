package provider

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
)

// Primary runtime of the home, the configured fallback chain walked when the primary fails on credentials
func ResolveRuntimeWithFallback(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*Runtime, map[string]any, error) {
	return modelsResolver(ctxForHome(ctx, h)).ResolveRuntimeWithFallback(requested, targetModel, explicitBaseURL, explicitAPIKey)
}

// Main model and provider of the home as the runtime resolver reads them, a local server's single model filling an unset model
func PrimaryModelRoute(ctx context.Context, h string) (model, provider string) {
	r := modelsResolver(ctxForHome(ctx, h))
	return r.cfgModel(), strings.TrimSpace(settings.ModelProvider.Get(r.hookCtx()))
}

// Named custom rung of the runtime resolver over the settings of ctx, a frozen launch config included, nil when the request is no custom route
func ResolveNamedCustomRuntime(ctx context.Context, h, requestedProvider, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	return modelsResolver(ctxForHome(ctx, h)).resolveNamedCustomRuntime(requestedProvider, explicitAPIKey, explicitBaseURL, targetModel)
}
