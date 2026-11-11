package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Every model key that belongs to one endpoint, all dropped together on a route change
var RouteBoundKeys = []string{"provider", "base_url", "api_mode", "context_length",
	"api_key", "api", "api_key_from", "key_env", "api_key_env"}

// Text of a config value as str(value or "") spells it
func routeStr(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Configured named endpoint entry a provider name selects in config without credentials, nil when none matches
func routeNamedEntry(ctx context.Context, name string, config map[string]any) map[string]any {
	return provider.NamedCustomProviderEntry(ctx, name, config)
}

// Canonical provider identity of a name, its normalized spelling when no registry provider answers it
func routeCanonical(ctx context.Context, name string) (string, error) {
	canonical := provider.NormalizeProvider(name)
	env := provider.HomePoolEnv(home.Dir(ctx))
	env.Ctx = ctx
	resolved, err := provider.ResolveAuthProvider(env, canonical)
	var authErr *provider.AuthError
	if errors.As(err, &authErr) {
		return canonical, nil
	}
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// Bare config key of the configured entry behind a custom identity, empty for anything else
func ConfiguredEntryKey(ctx context.Context, identity string, config map[string]any) string {
	identity = pystr.Lower(pystr.Strip(identity))
	if !strings.HasPrefix(identity, "custom:") || len(routeNamedEntry(ctx, identity, config)) == 0 {
		return ""
	}
	_, key, _ := strings.Cut(identity, ":")
	return pystr.Strip(key)
}

// Identity the profile's own model block resolves to, custom for an endpoint naming no provider
func ProfileRouteIdentity(ctx context.Context, config map[string]any) (string, error) {
	held := map[string]any{}
	if model, ok := config["model"].(map[string]any); ok {
		for key, value := range model {
			held[key] = value
		}
	}
	if name := pystr.Lower(pystr.Strip(routeStr(held["provider"]))); (name == "" || name == "auto") && pystr.Strip(routeStr(held["base_url"])) != "" {
		held["provider"] = "custom"
	}
	identity, _, err := ResolvedRoute(ctx, held, config)
	return identity, err
}

// Provider identity and effective base URL the runtime resolves a model block to
func ResolvedRoute(ctx context.Context, block any, config any) (string, string, error) {
	model, _ := block.(map[string]any)
	cfg, _ := config.(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	name := pystr.Lower(pystr.Strip(routeStr(model["provider"])))
	if name == "" || name == "auto" {
		return "auto", "", nil
	}
	baseURL := pystr.Strip(routeStr(model["base_url"]))
	var identity string
	if entry := routeNamedEntry(ctx, name, cfg); len(entry) > 0 {
		identity = provider.CustomProviderSlug(routeStr(entry["name"]), routeStr(entry["provider_key"]))
		if baseURL == "" {
			baseURL = pystr.Strip(routeStr(entry["base_url"]))
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
func SameRoute(ctx context.Context, a, b any, config any) (bool, error) {
	aID, aURL, err := ResolvedRoute(ctx, a, config)
	if err != nil {
		return false, err
	}
	bID, bURL, err := ResolvedRoute(ctx, b, config)
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
