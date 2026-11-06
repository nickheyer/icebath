package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Context bound to the given home, the context's own home when h is empty
func ctxForHome(ctx context.Context, h string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if h != "" {
		ctx = home.WithOverride(ctx, h)
	}
	return ctx
}

// TrialModel is the guest trial model while the home's Nous identity carries free inference, else empty
func TrialModel(ctx context.Context, h string) string {
	return trialModel(modelsPoolEnv(ctxForHome(ctx, h)))
}

// ResolveRuntimeProvider resolves the runtime of a requested provider for the home with explicit endpoint overrides
func ResolveRuntimeProvider(ctx context.Context, h, requested, targetModel, explicitBaseURL, explicitAPIKey string) (*Runtime, error) {
	return modelsResolver(ctxForHome(ctx, h)).ResolveRuntime(requested, explicitAPIKey, explicitBaseURL, nil, targetModel)
}

// ResolveFallbackEntryAPIKey is a fallback entry key, inline api_key else key_env through the home's secret scope
func ResolveFallbackEntryAPIKey(ctx context.Context, h string, entry map[string]any) string {
	return modelsResolver(ctxForHome(ctx, h)).ResolveEntryAPIKey(entry)
}
