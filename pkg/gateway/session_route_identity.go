package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Every model block key bound to one endpoint, dropped together when the route changes
var RouteBoundKeys = []string{"provider", "base_url", "api_mode", "context_length",
	"api_key", "api", "api_key_from", "key_env", "api_key_env"}

// Configured providers or custom_providers entry a name selects, without reading its credentials
func routeNamedEntry(ctx context.Context, name string, cfg map[string]any) map[string]any {
	return provider.NamedCustomProviderEntry(ctx, name, cfg)
}

// Canonical registry provider of a name, its normalized spelling when no registry provider matches
func routeCanonical(ctx context.Context, name string) (string, error) {
	canonical := provider.NormalizeProvider(name)
	h := home.Dir(ctx)
	env := provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Config: provider.ConfigLoader(h), Ctx: ctx}
	resolved, err := provider.ResolveAuthProvider(env, canonical)
	var authErr *provider.AuthError
	if errors.As(err, &authErr) {
		return canonical, nil
	}
	return resolved, err
}

// Bare config key of the configured entry behind a custom identity, empty for any other identity
func ConfiguredEntryKey(ctx context.Context, identity string, cfg map[string]any) string {
	identity = strings.ToLower(strings.TrimSpace(identity))
	if !strings.HasPrefix(identity, "custom:") || len(routeNamedEntry(ctx, identity, cfg)) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(identity, ":", 2)[1])
}

// Identity the profile model block resolves to, custom for a bare endpoint with no named provider
func ProfileRouteIdentity(ctx context.Context, cfg map[string]any) (string, error) {
	held := map[string]any{}
	if block, ok := cfg["model"].(map[string]any); ok {
		for k, v := range block {
			held[k] = v
		}
	}
	name := strings.ToLower(strings.TrimSpace(routeStr(held["provider"])))
	if (name == "" || name == "auto") && strings.TrimSpace(routeStr(held["base_url"])) != "" {
		held["provider"] = "custom"
	}
	identity, _, err := ResolvedRoute(ctx, held, cfg)
	return identity, err
}

// Provider identity and effective base url the runtime resolves a model block to
func ResolvedRoute(ctx context.Context, modelCfg, cfg any) (string, string, error) {
	block, _ := modelCfg.(map[string]any)
	tree, _ := cfg.(map[string]any)
	if tree == nil {
		tree = map[string]any{}
	}
	name := strings.ToLower(strings.TrimSpace(routeStr(block["provider"])))
	if name == "" || name == "auto" {
		return "auto", "", nil
	}
	baseURL := strings.TrimSpace(routeStr(block["base_url"]))
	var identity string
	if entry := routeNamedEntry(ctx, name, tree); len(entry) > 0 {
		identity = provider.CustomProviderSlug(routeStr(entry["name"]), routeStr(entry["provider_key"]))
		if baseURL == "" {
			baseURL = strings.TrimSpace(routeStr(entry["base_url"]))
		}
	} else {
		var err error
		if identity, err = routeCanonical(ctx, name); err != nil {
			return "", "", err
		}
	}
	return identity, strings.TrimRight(baseURL, "/"), nil
}

// Whether both model blocks resolve to one endpoint, auto on either side keeping the route
func SameRoute(ctx context.Context, a, b, cfg any) (bool, error) {
	aID, aURL, err := ResolvedRoute(ctx, a, cfg)
	if err != nil {
		return false, err
	}
	bID, bURL, err := ResolvedRoute(ctx, b, cfg)
	if err != nil {
		return false, err
	}
	if aID == "auto" || bID == "auto" {
		return true, nil
	}
	if aURL != "" || bURL != "" {
		return provider.NormalizeRouteBaseURL(aURL) == provider.NormalizeRouteBaseURL(bURL), nil
	}
	return aID == bID, nil
}

// Python str of a config value or the empty string for a falsy one
func routeStr(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}
