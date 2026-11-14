package catalog

import (
	"context"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	LlamaCppProviderID   = "llamacpp"
	transportOpenAIChat  = "openai_chat"
	authTypeAPIKey       = "api_key"
	sourceUserConfig     = "user-config"
	sourcePluginProfile  = "plugin-profile"
	sourceModelsDev      = "models.dev"
	sourceAuthRegistry   = "icebath-auth-registry"
	sourceLocalRuntime   = "local-runtime"
	customProviderPrefix = "custom:"
)

var LlamaCppAliases = []string{LlamaCppProviderID, "llama.cpp", "llama-cpp"}

var portalProviderNames = []string{providers.ProviderPortal, "icebath-portal"}

var builtinModes = []wire.Mode{wire.ModeChat, wire.ModeAnthropic, wire.ModeResponses, wire.ModeBedrock}

func (c *Catalog) provider(ctx context.Context, name string, allowNetwork bool) (providers.Definition, bool) {
	if !allowNetwork {
		ctx = cacheOnly(ctx)
	}
	return c.dir.Definition(ctx, name)
}

func (c *Catalog) Provider(ctx context.Context, name string, allowNetwork bool) (providers.Definition, bool) {
	return c.provider(c.scoped(ctx), name, allowNetwork)
}

func (c *Catalog) Label(ctx context.Context, providerID string) string {
	ctx = c.scoped(ctx)
	canonical := c.reg.NormalizeProvider(ctx, providerID)
	if label, ok := providers.LabelOverride(canonical); ok {
		return label
	}
	if def, ok := c.provider(ctx, canonical, true); ok {
		return def.Name
	}
	return canonical
}

func (c *Catalog) isAggregator(ctx context.Context, normalized string) bool {
	if strings.HasPrefix(normalized, customProviderPrefix) {
		return true
	}
	def, ok := c.provider(ctx, normalized, true)
	return ok && def.Aggregator
}

func (c *Catalog) IsAggregator(ctx context.Context, provider string) bool {
	ctx = c.scoped(ctx)
	return c.isAggregator(ctx, c.reg.NormalizeProvider(ctx, provider))
}

func (c *Catalog) IsRoutingAggregator(ctx context.Context, provider string) bool {
	ctx = c.scoped(ctx)
	normalized := c.reg.NormalizeProvider(ctx, provider)
	if providers.IsFlatNamespaceReseller(normalized) {
		return false
	}
	return c.isAggregator(ctx, normalized)
}

func (c *Catalog) DetermineMode(ctx context.Context, provider, baseURL, model string) wire.Mode {
	ctx = c.scoped(ctx)
	if c.reg.IsActualRoute(ctx, provider, baseURL) {
		return wire.ModeChat
	}
	if mode, ok := providers.HostMandatedMode(baseURL); ok {
		return mode
	}
	if slices.Contains(portalProviderNames, strings.ToLower(strings.TrimSpace(provider))) {
		return providers.PortalMode(model, providers.PortalAnthropicWire(c.config(ctx)))
	}
	if def, ok := c.provider(ctx, provider, true); ok {
		if mode, known := providers.TransportMode(def.Transport); known {
			return mode
		}
		dialect := wire.Mode(def.Transport)
		if slices.Contains(builtinModes, dialect) || slices.Contains(c.dialects(ctx), dialect) {
			return dialect
		}
		return wire.ModeChat
	}
	if provider == "bedrock" {
		return wire.ModeBedrock
	}
	return wire.ModeChat
}

func userDefinition(id, name, baseURL, keyEnv, transport string) providers.Definition {
	def := providers.Definition{ID: id, Name: name, Transport: transport, BaseURL: baseURL, AuthType: authTypeAPIKey, Source: sourceUserConfig}
	if keyEnv != "" {
		def.EnvVars = []string{keyEnv}
	}
	return def
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func ResolveUserProvider(name string, user config.Map[config.ProviderEntry]) (providers.Definition, bool) {
	entry, ok := user.Get(name)
	if !ok || !entry.Valid() {
		return providers.Definition{}, false
	}
	baseURL := firstNonEmpty(entry.API, entry.URL, entry.BaseURL)
	if baseURL == "" {
		return providers.Definition{}, false
	}
	return userDefinition(name, firstNonEmpty(entry.Name, name), baseURL, firstNonEmpty(entry.KeyEnv, entry.APIKeyEnv),
		firstNonEmpty(entry.Transport, transportOpenAIChat)), true
}

func CustomProviderSlug(displayName, providerKey string) string {
	identity := firstNonEmpty(strings.TrimSpace(providerKey), strings.TrimSpace(displayName))
	normalized := strings.ReplaceAll(strings.ToLower(identity), " ", "-")
	if strings.HasPrefix(normalized, customProviderPrefix) {
		return normalized
	}
	return customProviderPrefix + normalized
}

func CustomProviderAliases(displayName, providerKey string) []string {
	var aliases []string
	add := func(values ...string) {
		for _, value := range values {
			if !slices.Contains(aliases, value) {
				aliases = append(aliases, value)
			}
		}
	}
	for _, value := range []string{displayName, providerKey} {
		raw := strings.ToLower(strings.TrimSpace(value))
		if raw == "" {
			continue
		}
		normalized := strings.ReplaceAll(raw, " ", "-")
		add(raw, normalized, CustomProviderSlug(normalized, ""))
		if strings.HasPrefix(normalized, customProviderPrefix) {
			if _, suffix, _ := strings.Cut(normalized, ":"); suffix != "" {
				add(suffix, customProviderPrefix+normalized)
			}
		}
	}
	return aliases
}

func ResolveCustomProvider(name string, custom []config.CustomEndpoint) (providers.Definition, bool) {
	requested := strings.ToLower(strings.TrimSpace(name))
	if requested == "" || len(custom) == 0 {
		return providers.Definition{}, false
	}
	var first providers.Definition
	found := false
	for _, entry := range custom {
		displayName := strings.TrimSpace(entry.Text("name"))
		apiURL := strings.TrimSpace(firstNonEmpty(entry.Text("base_url"), entry.Text("url"), entry.Text("api")))
		if displayName == "" || apiURL == "" {
			continue
		}
		providerKey := strings.TrimSpace(entry.Text("provider_key"))
		def := userDefinition(CustomProviderSlug(displayName, providerKey), displayName, apiURL, strings.TrimSpace(entry.Text("key_env")), transportOpenAIChat)
		if !found {
			first, found = def, true
		}
		if slices.Contains(CustomProviderAliases(displayName, providerKey), requested) {
			return def, true
		}
	}
	if requested == "custom" && found {
		return first, true
	}
	return providers.Definition{}, false
}

func (c *Catalog) profileDefinition(ctx context.Context, name string) (providers.Definition, bool) {
	p, ok := c.reg.Get(ctx, name)
	if !ok {
		return providers.Definition{}, false
	}
	spec := p.Spec()
	transport, known := providers.ModeTransport(spec.Mode)
	if !known {
		transport = firstNonEmpty(strings.TrimSpace(string(spec.Mode)), transportOpenAIChat)
	}
	return providers.Definition{
		ID: spec.Name, Name: firstNonEmpty(spec.DisplayName, spec.Name, name), Transport: transport, EnvVars: spec.KeyEnvVars(),
		BaseURL: strings.TrimSpace(spec.BaseURL), BaseURLEnv: spec.URLEnvVar(), AuthType: spec.AuthTypeOrDefault(), Source: sourcePluginProfile,
	}, true
}

func (c *Catalog) overlayProfileOwns(ctx context.Context, canonical string) bool {
	if canonical == "" {
		return false
	}
	for _, p := range c.reg.All(ctx) {
		spec := p.Spec()
		if spec.Overlay != nil && (canonical == spec.Name || canonical == spec.ModelsDevID || slices.Contains(spec.OverlayAliases, canonical)) {
			return true
		}
	}
	return false
}

func (c *Catalog) registryKeys(ctx context.Context) []string {
	var keys []string
	add := func(key string) {
		if key == "" || slices.Contains(keys, key) {
			return
		}
		if _, ok := c.reg.RegistryRow(ctx, key); ok {
			keys = append(keys, key)
		}
	}
	for _, row := range c.reg.RegistryRows(ctx) {
		add(row.ID)
	}
	for _, p := range c.reg.All(ctx) {
		spec := p.Spec()
		add(spec.Name)
		for _, alias := range spec.Aliases {
			add(alias)
		}
	}
	return keys
}

func (c *Catalog) lossyAliasRegistryDefinition(ctx context.Context, raw, canonical string) (providers.Definition, bool) {
	row, ok := c.reg.RegistryRow(ctx, raw)
	if !ok {
		return providers.Definition{}, false
	}
	matches, same := 0, true
	for _, key := range c.registryKeys(ctx) {
		if c.reg.NormalizeProvider(ctx, key) != canonical {
			continue
		}
		matches++
		if other, _ := c.reg.RegistryRow(ctx, key); other.ID != row.ID {
			same = false
		}
	}
	if same && c.overlayProfileOwns(ctx, canonical) {
		return providers.Definition{}, false
	}
	if matches > 1 {
		return providers.Definition{
			ID: row.ID, Name: row.Name, Transport: transportOpenAIChat, EnvVars: slices.Clone(row.APIKeyEnvVars),
			BaseURL: row.InferenceBaseURL, AuthType: authTypeAPIKey, Source: sourceAuthRegistry,
		}, true
	}
	return providers.Definition{}, false
}

func (c *Catalog) llamaCppDefinition(ctx context.Context) (providers.Definition, bool) {
	endpoint := c.local.ResolveLlamaCppEndpoint(ctx, 0)
	if endpoint == nil && len(c.local.StagedModelIDs()) == 0 {
		return providers.Definition{}, false
	}
	def := providers.Definition{ID: LlamaCppProviderID, Name: "Local", Transport: transportOpenAIChat, AuthType: authTypeAPIKey, Source: sourceLocalRuntime}
	if endpoint != nil {
		def.BaseURL = endpoint.BaseURL
	}
	return def, true
}

func (c *Catalog) ResolveProviderFull(ctx context.Context, name string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) (providers.Definition, bool) {
	ctx = c.scoped(ctx)
	canonical := c.reg.NormalizeProvider(ctx, name)
	raw := strings.ToLower(strings.TrimSpace(name))
	hasUser := user.Len() > 0
	if hasUser {
		if def, ok := ResolveUserProvider(raw, user); ok {
			return def, true
		}
	}
	if canonical != raw {
		if def, ok := c.lossyAliasRegistryDefinition(ctx, raw, canonical); ok {
			return def, true
		}
	}
	if def, ok := c.provider(ctx, canonical, true); ok {
		if def.Source == sourcePluginProfile && hasUser {
			if userDef, ok := ResolveUserProvider(def.ID, user); ok {
				return userDef, true
			}
		}
		return def, true
	}
	if hasUser {
		for _, candidate := range []string{canonical, raw} {
			if def, ok := ResolveUserProvider(candidate, user); ok {
				return def, true
			}
		}
	}
	if def, ok := ResolveCustomProvider(name, custom); ok {
		return def, true
	}
	if slices.Contains(LlamaCppAliases, raw) {
		if def, ok := c.llamaCppDefinition(ctx); ok {
			return def, true
		}
	}
	if info, ok := c.providerInfo(ctx, canonical, true, nil); ok {
		return providers.Definition{
			ID: canonical, Name: info.Name, Transport: transportOpenAIChat, EnvVars: info.Env, BaseURL: info.API,
			AuthType: authTypeAPIKey, Source: sourceModelsDev,
		}, true
	}
	if def, ok := c.profileDefinition(ctx, canonical); ok && def.ID != "custom" {
		return def, true
	}
	return providers.Definition{}, false
}
