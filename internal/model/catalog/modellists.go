package catalog

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	portalModelsTimeout   = 15 * time.Second
	anthropicListTimeout  = 5 * time.Second
	aiGatewayListTimeout  = 5 * time.Second
	openAIDefaultBaseURL  = "https://api.openai.com/v1"
	providerOllama        = "ollama"
	providerCustom        = "custom"
	providerOpenRouter    = "openrouter"
	providerAzureFoundry  = providers.ProviderAzureFoundry
	modelListDebugMessage = "model catalog source declined"
)

var relayAwareFetchers = []string{"anthropic", "custom", "openai", "openai-api", "stepfun", "gmi"}

var openCodeFreeExcluded = []string{"ox-alpha-free", "deepseek-v4-flash-free", "x-preview-f-free"}

func lowerKey(id string) string { return strings.ToLower(id) }

func modelDedupKey(id string) string {
	return modelAliasCanonical(strings.ToLower(strings.TrimSpace(id)))
}

func mergeUnique(primary, secondary []string, key func(string) string) []string {
	merged := slices.Clone(primary)
	seen := make(map[string]bool, len(primary))
	for _, id := range primary {
		seen[key(id)] = true
	}
	for _, id := range secondary {
		if k := key(id); !seen[k] {
			seen[k] = true
			merged = append(merged, id)
		}
	}
	return merged
}

func (c *Catalog) mergeWithModelsDev(ctx context.Context, provider string, curated []string) []string {
	registry := c.ListAgenticModels(ctx, provider, true)
	if len(registry) == 0 {
		return slices.Clone(curated)
	}
	return mergeUnique(mergeUnique(nil, registry, lowerKey), curated, lowerKey)
}

func (c *Catalog) openAIDiscoveryBaseURL(ctx context.Context, provider string) string {
	if override := strings.TrimRight(c.env(ctx, "OPENAI_BASE_URL"), "/"); override != "" {
		return override
	}
	model := c.modelConfig(ctx)
	configured := strings.ToLower(strings.TrimSpace(model.Provider))
	if (configured == "openai" || configured == "openai-api") && c.normalizeProvider(ctx, provider) == c.normalizeProvider(ctx, configured) {
		if base := strings.TrimRight(strings.TrimSpace(model.BaseURL), "/"); base != "" {
			return base
		}
	}
	return openAIDefaultBaseURL
}

func (c *Catalog) portalCatalog(ctx context.Context) ([]string, bool) {
	if creds, err := c.portal.RuntimeCredentials(portal.WithBackground(ctx), portal.ResolveOptions{}); err == nil {
		if live, err := c.portal.FetchModels(ctx, creds.BaseURL, creds.APIKey, portalModelsTimeout); err == nil && len(live) > 0 {
			return live, true
		}
	}
	curated := c.curatedPortalModelIDs(ctx)
	return curated, len(curated) > 0
}

func (c *Catalog) apiKeyCredentials(ctx context.Context, provider string) (auth.Secret, string) {
	creds, err := c.auth.ResolveAPIKey(ctx, provider)
	if err != nil {
		return auth.Secret{}, ""
	}
	return auth.NewSecret(strings.TrimSpace(creds.APIKey.Reveal())), strings.TrimSpace(creds.BaseURL)
}

func (c *Catalog) apiKeyProviderLive(ctx context.Context, provider string) ([]string, bool) {
	apiKey, baseURL := c.apiKeyCredentials(ctx, provider)
	if apiKey.Empty() || baseURL == "" {
		return nil, false
	}
	live, _ := c.fetchAPIModels(ctx, apiKey, baseURL, apiModelsTimeout, "", nil)
	return live, len(live) > 0
}

func (c *Catalog) stepfunCatalog(ctx context.Context, provider string) ([]string, bool) {
	live, ok := c.apiKeyProviderLive(ctx, provider)
	if !ok {
		return nil, false
	}
	if curated := c.staticCatalog(ctx, provider); len(curated) > 0 {
		return mergeUnique(live, curated, modelDedupKey), true
	}
	return live, true
}

func (c *Catalog) anthropicCatalog(ctx context.Context) listing {
	model := c.modelConfig(ctx)
	baseURL, apiKey := "", ""
	if c.normalizeProvider(ctx, model.Provider) == "anthropic" {
		baseURL, apiKey = strings.TrimSpace(model.BaseURL), strings.TrimSpace(model.APIKey)
	}
	live, _ := c.fetchAnthropicModels(ctx, anthropicListTimeout, baseURL, auth.NewSecret(apiKey))
	curated, _ := c.builtinStaticRow(ctx, "anthropic")
	if len(live) == 0 {
		return listing{ids: slices.Clone(curated), fallback: true}
	}
	if baseURL != "" {
		return listing{ids: live}
	}
	return listing{ids: mergeUnique(curated, live, lowerKey)}
}

func (c *Catalog) openAICatalog(ctx context.Context, provider string) ([]string, bool) {
	apiKey := c.env(ctx, "OPENAI_API_KEY")
	if apiKey == "" {
		return nil, false
	}
	base := c.openAIDiscoveryBaseURL(ctx, provider)
	live, _ := c.fetchAPIModels(ctx, auth.NewSecret(apiKey), base, apiModelsTimeout, "", nil)
	if len(live) == 0 {
		return nil, false
	}
	if !providers.IsOfficialOpenAIHost(base) {
		return live, true
	}
	curated := c.staticCatalog(ctx, provider)
	discovered := []string{}
	for _, id := range curated {
		if slices.ContainsFunc(live, func(listed string) bool { return strings.EqualFold(listed, id) }) {
			discovered = append(discovered, id)
		}
	}
	for _, id := range live {
		if wire.IsAstraModel(id) {
			discovered = append(discovered, id)
		}
	}
	switch {
	case len(discovered) > 0:
		return discovered, true
	case len(curated) > 0:
		return slices.Clone(curated), true
	}
	return live, true
}

func (c *Catalog) customBaseURL(ctx context.Context) string {
	return strings.TrimSpace(c.modelConfig(ctx).BaseURL)
}

func (c *Catalog) customCatalog(ctx context.Context) ([]string, bool) {
	baseURL := c.customBaseURL(ctx)
	if baseURL == "" {
		return nil, false
	}
	apiKey := strings.TrimSpace(c.modelConfig(ctx).APIKey)
	for _, name := range []string{"CUSTOM_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		if apiKey != "" {
			break
		}
		apiKey = c.env(ctx, name)
	}
	mode := ""
	if baseURLLooksLikeAnthropicMessages(baseURL) {
		mode = ModeAnthropicMessages
	}
	live, _ := c.fetchAPIModels(ctx, auth.NewSecret(apiKey), baseURL, apiModelsTimeout, mode, nil)
	return live, len(live) > 0
}

func (c *Catalog) bedrockCatalog(ctx context.Context) ([]string, bool) {
	owner, ok := c.reg.BedrockOwner()
	if !ok {
		return nil, false
	}
	access, err := owner.Bedrock(ctx, true)
	if err != nil {
		return nil, false
	}
	return providers.BedrockModelIDs(ctx, access.RuntimeRegion(), func(ctx context.Context, region string) ([]string, error) {
		discovered := c.DiscoverBedrockModels(ctx, region, nil)
		ids := make([]string, 0, len(discovered))
		for _, model := range discovered {
			ids = append(ids, model.ID)
		}
		return ids, nil
	})
}

func (c *Catalog) azureFoundryCatalog(ctx context.Context, provider string) ([]string, bool) {
	route, err := c.routes.ResolveRoute(ctx, provider, "")
	if err != nil {
		return nil, false
	}
	baseURL := strings.TrimRight(strings.TrimSpace(route.BaseURL), "/")
	if baseURL == "" || route.Key.Kind == auth.CredNone {
		return nil, false
	}
	n, err := c.net(ctx, baseURL, apiModelsTimeout)
	if err != nil {
		return nil, false
	}
	ok, ids := providers.ProbeAzureModels(ctx, n, baseURL, route.Key)
	return ids, ok && len(ids) > 0
}

func (c *Catalog) ollamaCloudCatalog(ctx context.Context, force bool) ([]string, bool) {
	models := c.local.FetchOllamaCloudModels(ctx, local.OllamaCloudRequest{
		ForceRefresh: force,
		List:         c.ListAPIModels,
		ModelsDev: func(ctx context.Context) ([]string, error) {
			return c.ListAgenticModels(ctx, "ollama-cloud", true), nil
		},
	})
	return models, len(models) > 0
}

func (c *Catalog) hasCatalogFetcher(ctx context.Context, normalized string) bool {
	switch normalized {
	case providerOpenRouter, "openai-codex", providers.ProviderPortal, "stepfun", "gmi", "anthropic", "ai-gateway", "deepinfra",
		"ollama-cloud", "openai", "openai-api", providerCustom, "bedrock", providerAzureFoundry:
		return true
	}
	if p, ok := c.reg.Get(ctx, normalized); ok {
		_, hooked := p.(providers.PickerCataloger)
		return hooked
	}
	return false
}

func (c *Catalog) catalogFetch(ctx context.Context, normalized string, force bool) (listing, bool) {
	plain := func(ids []string, ok bool) (listing, bool) { return listing{ids: ids}, ok }
	switch normalized {
	case providerOpenRouter:
		return listing{ids: choiceIDs(c.fetchOpenRouterModels(ctx, openRouterListTimeout, force, false))}, true
	case "openai-codex":
		return c.codexCatalog(ctx), true
	case providers.ProviderPortal:
		return plain(c.portalCatalog(ctx))
	case "stepfun":
		return plain(c.stepfunCatalog(ctx, normalized))
	case "gmi":
		return plain(c.apiKeyProviderLive(ctx, normalized))
	case "anthropic":
		return c.anthropicCatalog(ctx), true
	case "ai-gateway":
		ids, _ := c.fetchAIGatewayLanguageModels(ctx, aiGatewayListTimeout)
		return listing{ids: ids}, len(ids) > 0
	case "deepinfra":
		ids, _ := c.fetchDeepInfraModels(ctx, deepInfraTimeout, force)
		if ids == nil {
			ids = []string{}
		}
		return listing{ids: ids}, true
	case "ollama-cloud":
		return plain(c.ollamaCloudCatalog(ctx, force))
	case "openai", "openai-api":
		return plain(c.openAICatalog(ctx, normalized))
	case providerCustom:
		return plain(c.customCatalog(ctx))
	case "bedrock":
		return plain(c.bedrockCatalog(ctx))
	case providerAzureFoundry:
		return plain(c.azureFoundryCatalog(ctx, normalized))
	}
	p, ok := c.reg.Get(ctx, normalized)
	if !ok {
		return listing{}, false
	}
	hook, ok := p.(providers.PickerCataloger)
	if !ok {
		return listing{}, false
	}
	ids, listed, err := hook.PickerCatalog(ctx, force)
	if err != nil {
		slog.Debug(modelListDebugMessage, "provider", normalized, "error", err)
		return listing{}, false
	}
	return listing{ids: ids}, listed
}

func (c *Catalog) dropDelistedOpenCodeModels(normalized string, rows listing) listing {
	if len(rows.ids) == 0 || !slices.Contains(liveFirstPickerProviders, normalized) {
		return rows
	}
	kept := make([]string, 0, len(rows.ids))
	for _, id := range rows.ids {
		if !slices.Contains(openCodeFreeExcluded, strings.ToLower(id)) {
			kept = append(kept, id)
		}
	}
	rows.ids = kept
	return rows
}

func (c *Catalog) mergeProfileCatalog(ctx context.Context, normalized string, spec *providers.Spec, live []string) (listing, bool) {
	var rows listing
	listed := true
	switch {
	case len(live) == 0 && len(spec.FallbackModels) == 0:
		listed = false
	case len(live) == 0:
		rows = listing{ids: slices.Clone(spec.FallbackModels), fallback: true}
	default:
		curated := c.staticCatalog(ctx, normalized)
		if len(curated) == 0 {
			curated = spec.FallbackModels
		}
		switch {
		case len(curated) == 0:
			rows = listing{ids: live}
		case slices.Contains(liveFirstPickerProviders, normalized):
			rows = listing{ids: mergeUnique(live, curated, modelDedupKey)}
		default:
			rows = listing{ids: mergeUnique(curated, live, modelDedupKey)}
		}
	}
	if !listed {
		return listing{}, false
	}
	return c.dropDelistedOpenCodeModels(normalized, rows), true
}

func (c *Catalog) MergeProfileCatalog(ctx context.Context, provider string, live []string) (ids []string, fallback, ok bool) {
	ctx = c.scoped(ctx)
	p, found := c.reg.Get(ctx, provider)
	if !found {
		return nil, false, false
	}
	rows, ok := c.mergeProfileCatalog(ctx, provider, p.Spec(), live)
	return rows.ids, rows.fallback, ok
}

func (c *Catalog) listProfileModels(ctx context.Context, p providers.Profile, apiKey auth.Secret, baseURL string) []string {
	n, err := c.net(ctx, baseURL, openRouterListTimeout)
	if err != nil {
		return nil
	}
	live, _ := providers.ListModels(ctx, n, p, providers.ListInput{APIKey: apiKey, BaseURL: baseURL, ChatIDs: c.ChatCatalogIDs})
	return live
}

func (c *Catalog) probeProfileCatalog(ctx context.Context, normalized string, p providers.Profile, apiKey auth.Secret, baseURL string) (listing, bool) {
	var live []string
	if !apiKey.Empty() {
		live = c.listProfileModels(ctx, p, apiKey, baseURL)
	}
	return c.mergeProfileCatalog(ctx, normalized, p.Spec(), live)
}

func (c *Catalog) ProbeProfileCatalog(ctx context.Context, provider string, apiKey auth.Secret, baseURL string) (ids []string, fallback, ok bool) {
	ctx = c.scoped(ctx)
	p, found := c.reg.Get(ctx, provider)
	if !found {
		return nil, false, false
	}
	rows, ok := c.probeProfileCatalog(ctx, provider, p, apiKey, baseURL)
	return rows.ids, rows.fallback, ok
}

func (c *Catalog) profileLiveCatalog(ctx context.Context, normalized string) (listing, bool) {
	p, ok := c.reg.Get(ctx, normalized)
	if !ok {
		return listing{}, false
	}
	spec := p.Spec()
	authType := spec.AuthTypeOrDefault()
	if authType == auth.AuthTypeExternalProcess {
		return c.mergeProfileCatalog(ctx, normalized, spec, c.listProfileModels(ctx, p, auth.Secret{}, ""))
	}
	if authType != auth.AuthTypeAPIKey || spec.BaseURL == "" {
		return listing{ids: slices.Clone(spec.FallbackModels)}, len(spec.FallbackModels) > 0
	}
	apiKey, baseURL := c.apiKeyCredentials(ctx, normalized)
	if baseURL == "" {
		baseURL = spec.BaseURL
	}
	return c.probeProfileCatalog(ctx, normalized, p, apiKey, baseURL)
}

func (c *Catalog) chatRows(rows listing) listing {
	rows.ids = c.WithoutGenerationModels(rows.ids)
	return rows
}

func (c *Catalog) configuredRelayBaseURL(ctx context.Context, provider string) string {
	model := c.modelConfig(ctx)
	configured := strings.ToLower(strings.TrimSpace(model.Provider))
	if configured == "" || provider == "" {
		return ""
	}
	normalized := c.normalizeProvider(ctx, provider)
	if normalized != c.normalizeProvider(ctx, configured) {
		return ""
	}
	baseURL := strings.TrimRight(strings.TrimSpace(model.BaseURL), "/")
	if baseURL == "" {
		return ""
	}
	var canonical []string
	if p, ok := c.reg.Get(ctx, normalized); ok && p.Spec().BaseURL != "" {
		canonical = append(canonical, NormalizeRouteBaseURL(p.Spec().BaseURL))
	}
	if row, ok := c.reg.RegistryRow(ctx, normalized); ok && row.InferenceBaseURL != "" {
		canonical = append(canonical, NormalizeRouteBaseURL(row.InferenceBaseURL))
	}
	if slices.Contains(canonical, NormalizeRouteBaseURL(baseURL)) {
		return ""
	}
	return baseURL
}

func (c *Catalog) relayModelCatalog(ctx context.Context, normalized, relay string) ([]string, bool) {
	p, ok := c.reg.Get(ctx, normalized)
	if !ok || p.Spec().AuthTypeOrDefault() != auth.AuthTypeAPIKey {
		return nil, false
	}
	apiKey, _ := c.apiKeyCredentials(ctx, normalized)
	var live []string
	for _, id := range c.listProfileModels(ctx, p, apiKey, relay) {
		if id != "" {
			live = append(live, id)
		}
	}
	return live, len(live) > 0
}

func (c *Catalog) staticCatalogTail(ctx context.Context, normalized string, hasFetcher bool) listing {
	curated := listing{ids: slices.Clone(c.staticCatalog(ctx, normalized)), fallback: hasFetcher}
	if !slices.Contains(modelsDevPreferred, normalized) {
		return c.chatRows(c.dropDelistedOpenCodeModels(normalized, curated))
	}
	merged := c.dropDelistedOpenCodeModels(normalized, listing{ids: c.mergeWithModelsDev(ctx, normalized, curated.ids)})
	if slices.Contains(xaiRows, normalized) {
		merged.ids = xaiFinalizeCatalog(merged.ids)
	}
	return c.chatRows(merged)
}

func (c *Catalog) providerModelIDs(ctx context.Context, provider string, force bool) (listing, error) {
	if strings.ToLower(strings.TrimSpace(provider)) == providerOllama {
		models, err := c.local.OllamaLocalCatalog(ctx, force, c.ListAPIModels)
		if err != nil {
			return listing{}, err
		}
		return listing{ids: models}, nil
	}
	normalized := c.normalizeProvider(ctx, provider)
	if relay := c.configuredRelayBaseURL(ctx, provider); relay != "" && !slices.Contains(relayAwareFetchers, normalized) {
		if relayed, ok := c.relayModelCatalog(ctx, normalized, relay); ok {
			return c.chatRows(listing{ids: relayed}), nil
		}
		return c.staticCatalogTail(ctx, normalized, c.hasCatalogFetcher(ctx, normalized)), nil
	}
	hasFetcher := c.hasCatalogFetcher(ctx, normalized)
	if hasFetcher {
		if models, ok := c.catalogFetch(ctx, normalized, force); ok {
			return c.chatRows(models), nil
		}
	}
	if models, ok := c.profileLiveCatalog(ctx, normalized); ok {
		return c.chatRows(models), nil
	}
	return c.staticCatalogTail(ctx, normalized, hasFetcher), nil
}

func (c *Catalog) ProviderModelIDs(ctx context.Context, provider string, force bool) ([]string, error) {
	rows, err := c.providerModelIDs(c.scoped(ctx), provider, force)
	return rows.ids, err
}
