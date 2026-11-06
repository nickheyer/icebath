package provider

import (
	"context"

	"github.com/nickheyer/icebath/pkg/config"
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

// Resolver over the home's config, failing with the read error a malformed config raises
func strictHomeResolver(ctx context.Context, h string) (*resolver, map[string]any, error) {
	ctx = ctxForHome(ctx, h)
	hd := home.Dir(ctx)
	cfg := FrozenRuntimeConfig()
	if cfg == nil {
		tree, err := config.Load(hd, config.NewSecrets(hd).Lookup())
		if err != nil {
			return nil, nil, err
		}
		cfg = map[string]any(tree)
	}
	return newResolver(ctx, config.Tree(cfg), config.NewSecrets(hd), hd, homeCredentialSource(hd)), cfg, nil
}

// HostGatedEnvKeyCandidates lists the env keys a base URL host would receive, then its host derived vendor key
func HostGatedEnvKeyCandidates(ctx context.Context, h, baseURL string, ollama bool) []string {
	return modelsResolver(ctxForHome(ctx, h)).hostGatedEnvKeyCandidates(baseURL, ollama)
}

// HasNamedCustomProvider reports whether the home's config defines a providers or custom_providers entry for the name
func HasNamedCustomProvider(ctx context.Context, h, name string) bool {
	ctx = ctxForHome(ctx, h)
	return namedCustomProviderConfigured(ctx, name, home.Dir(ctx))
}

// NamedCustomProvider is the resolved named endpoint entry with credentials, nil when no entry matches
func NamedCustomProvider(ctx context.Context, h, name string) (map[string]any, error) {
	r, cfg, err := strictHomeResolver(ctx, h)
	if err != nil {
		return nil, err
	}
	return r.getNamedCustomProvider(name, cfg, true), nil
}

// ResolveRequestedProvider is the explicit provider, then the config provider, then ICEBATH_INFERENCE_PROVIDER, else auto
func ResolveRequestedProvider(ctx context.Context, h, requested string) (string, error) {
	r, _, err := strictHomeResolver(ctx, h)
	if err != nil {
		return "", err
	}
	return r.resolveRequestedProvider(requested), nil
}

// Static inference base URL of a registry row, empty for Vertex whose endpoint is computed per request
func registryInferenceBaseURL(p *Profile) string {
	if p.AuthType == AuthTypeVertex {
		return ""
	}
	return p.BaseURL
}

// RegistryInferenceBaseURL is the inference base URL of a provider registry id and whether the id is registered
func RegistryInferenceBaseURL(id string) (string, bool) {
	row := providerRegistry().get(context.Background(), id)
	if row == nil {
		return "", false
	}
	return registryInferenceBaseURL(row), true
}

// RegistryInferenceBaseURLs lists every provider registry entry's inference base URL in registry order
func RegistryInferenceBaseURLs() []string {
	var out []string
	for _, it := range providerRegistry().items(context.Background()) {
		out = append(out, registryInferenceBaseURL(it.Row))
	}
	return out
}
