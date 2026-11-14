package model

import (
	"context"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/catalog"
	"github.com/nickheyer/icebath/internal/model/providers"
)

func (r *Router) legacyEndpoints(cfg *config.Config) []config.CustomEndpoint {
	if cfg.Extra.CustomProviders.IsZero() {
		return nil
	}
	node, err := config.ValueNode(cfg.Extra.CustomProviders)
	if err != nil || node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	tree := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "custom_providers"}, node,
	}}
	return r.c.o.Config.CustomEndpointsOf(tree)
}

func (r *Router) OwnsRoute(ctx context.Context, provider, baseURL string, cfg *config.Config) (owns, known bool) {
	ctx = r.bind(ctx)
	host := providers.URLHostname(baseURL)
	if host == "" {
		return false, false
	}
	reg, cat := r.c.o.Providers, r.c.o.Catalog
	raw := strings.ToLower(strings.TrimSpace(provider))
	canonical := reg.NormalizeProvider(ctx, raw)
	if canonical == "custom" || canonical == "local" {
		return true, true
	}
	var user providers.Definition
	hasUser := false
	if cfg != nil {
		if user, hasUser = catalog.ResolveUserProvider(raw, cfg.Providers); !hasUser {
			user, hasUser = catalog.ResolveCustomProvider(raw, r.legacyEndpoints(cfg))
		}
	}
	if hasUser && user.BaseURL != "" {
		return providers.URLHostname(user.BaseURL) == host, true
	}
	def, hasDef := cat.Provider(ctx, canonical, false)
	if hasDef && def.BaseURL != "" && providers.URLHostname(def.BaseURL) == host {
		return true, true
	}
	if !hasDef && !hasUser {
		return true, true
	}
	inferred, ok := cat.InferProviderFromURL(ctx, baseURL)
	if !ok {
		return false, false
	}
	return reg.NormalizeProvider(ctx, inferred) == canonical, true
}

func (r *Router) IsForeignProviderEndpoint(ctx context.Context, provider, baseURL string) bool {
	ctx = r.bind(ctx)
	reg := r.c.o.Providers
	row, ok := reg.RegistryRow(ctx, strings.ToLower(strings.TrimSpace(provider)))
	url := trimURL(baseURL)
	if !ok || url == "" || url == strings.TrimRight(row.InferenceBaseURL, "/") {
		return false
	}
	for _, other := range reg.RegistryRows(ctx) {
		if url == strings.TrimRight(other.InferenceBaseURL, "/") {
			return true
		}
	}
	return false
}

func (r *Router) providerDefaultRoutes(ctx context.Context, provider string) map[string]bool {
	routes := map[string]bool{}
	add := func(v string) {
		if route := catalog.NormalizeRouteBaseURL(v); route != "" {
			routes[route] = true
		}
	}
	reg := r.c.o.Providers
	if ov, ok := reg.Overlays(ctx)[provider]; ok {
		add(ov.BaseURLOverride)
	}
	if def, ok := r.c.o.Catalog.Provider(ctx, provider, false); ok {
		add(def.BaseURL)
	}
	if p, ok := reg.Get(ctx, provider); ok {
		add(p.Spec().BaseURL)
	}
	for _, row := range reg.RegistryRows(ctx) {
		if reg.NormalizeProvider(ctx, modelsProvider(row.ID)) == provider {
			add(row.InferenceBaseURL)
		}
	}
	if provider == "gemini" {
		base := make([]string, 0, len(routes))
		for route := range routes {
			base = append(base, route)
		}
		for _, route := range base {
			routes[strings.TrimRight(route, "/")+"/openai"] = true
		}
	}
	return routes
}

func (r *Router) ContextRouteMismatch(ctx context.Context, configuredBaseURL, activeBaseURL, configuredProvider, activeProvider string, alreadyNormalized bool) bool {
	ctx = r.bind(ctx)
	norm := catalog.NormalizeRouteBaseURL
	if alreadyNormalized {
		norm = func(v string) string { return v }
	}
	configuredRoute, activeRoute := norm(configuredBaseURL), norm(activeBaseURL)
	if configuredRoute != "" {
		return configuredRoute != activeRoute
	}
	cp, ap := strings.TrimSpace(configuredProvider), strings.TrimSpace(activeProvider)
	if cp == "" {
		return false
	}
	reg := r.c.o.Providers
	cp, ap = reg.NormalizeProvider(ctx, modelsProvider(cp)), reg.NormalizeProvider(ctx, modelsProvider(ap))
	if activeRoute != "" {
		if routes := r.providerDefaultRoutes(ctx, cp); len(routes) > 0 {
			return !routes[activeRoute]
		}
		return !(ap != "" && cp == ap)
	}
	return cp != "" && ap != "" && cp != ap
}

func (r *Router) ShouldClearContextPin(ctx context.Context, configuredModel, activeModel, configuredBaseURL, activeBaseURL, configuredProvider, activeProvider string) bool {
	if m := strings.TrimSpace(configuredModel); m != "" && m != strings.TrimSpace(activeModel) {
		return true
	}
	return r.ContextRouteMismatch(ctx, configuredBaseURL, activeBaseURL, configuredProvider, activeProvider, false)
}
