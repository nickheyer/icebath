package catalog

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/httpclient"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	bedrockProbeFailureTTL = 300 * time.Second
	defaultMoAPreset       = "default"
	novitaBaseURL          = "https://api.novita.ai/openai/v1"
	anthropicBaseURL       = "https://api.anthropic.com"
	anthropicOAuthPrefix   = "sk-ant-oat"
)

var bedrockRuntimeRegion = regexp.MustCompile(`bedrock-runtime\.([a-z0-9-]+)\.`)

type ContextQuery struct {
	Model      string
	BaseURL    string
	Key        auth.Credential
	Configured int
	Provider   string
	Custom     []config.CustomEndpoint
}

type bedrockProbeKey struct {
	baseURL string
	model   string
	region  string
}

func (c *Catalog) warnContextLengthFallback(model, baseURL string) {
	if !c.once("fallback\x00" + model + "\x00" + baseURL) {
		return
	}
	shown := shownURL(baseURL)
	if shown == "" {
		shown = "default"
	}
	slog.Warn("Could not determine context length, falling back to the default window. Set model.context_length in config.yaml to override.",
		"model", model, "base_url", shown, "tokens", groupedInt(DefaultFallbackContext))
}

func skipPersistentContextCache(provider string) bool {
	p := strings.ToLower(strings.TrimSpace(provider))
	return p == "lmstudio" || p == "openai-codex"
}

func (c *Catalog) isCodexRoute(ctx context.Context, provider, baseURL string, custom []config.CustomEndpoint) bool {
	if strings.ToLower(strings.TrimSpace(provider)) == "openai-codex" {
		return true
	}
	if baseURL == "" {
		return false
	}
	return c.customAPIMode(ctx, baseURL, custom) == "codex_responses"
}

func (c *Catalog) saveUnlessSkipped(ctx context.Context, model, baseURL string, window int, provider string) {
	if !skipPersistentContextCache(provider) {
		c.saveContextLength(ctx, model, baseURL, window, "")
	}
}

func (c *Catalog) maybeCacheLocalContextLength(ctx context.Context, model, baseURL string, window int) {
	if window >= MinimumContextLength {
		c.saveContextLength(ctx, model, baseURL, window, "")
	}
}

func (c *Catalog) probeLocalContextLength(ctx context.Context, model, baseURL string, key auth.Credential, provider string) (int, bool) {
	window, ok := c.localContextLength(ctx, model, baseURL, key)
	if !ok || window <= 0 {
		return 0, false
	}
	if !skipPersistentContextCache(provider) {
		c.maybeCacheLocalContextLength(ctx, model, baseURL, window)
	}
	return window, true
}

func (c *Catalog) reconcileLocalCachedContextLength(ctx context.Context, model, baseURL string, cached int, key auth.Credential) int {
	live, ok := c.localContextLength(ctx, model, baseURL, key)
	if !ok || live <= 0 || live == cached {
		return cached
	}
	if live < MinimumContextLength {
		slog.Info("live local probe reports a window under the minimum, invalidating the stale cache entry", "model", model, "base_url", shownURL(baseURL),
			"tokens", groupedInt(live), "minimum", groupedInt(MinimumContextLength))
	} else {
		slog.Info("reconciling a stale local cache entry with the live probe", "model", model, "base_url", shownURL(baseURL),
			"cached", groupedInt(cached), "live", groupedInt(live))
	}
	c.invalidateCachedContextLength(ctx, model, baseURL)
	if live >= MinimumContextLength {
		c.maybeCacheLocalContextLength(ctx, model, baseURL, live)
	}
	return live
}

func (c *Catalog) anthropicContextLength(ctx context.Context, model, baseURL string, key auth.Credential) (int, bool) {
	secret := key.Key.Reveal()
	if key.Tokens != nil || secret == "" || strings.HasPrefix(secret, anthropicOAuthPrefix) {
		return 0, false
	}
	base := strings.TrimSuffix(strings.TrimRight(baseURL, "/"), "/v1")
	header := http.Header{}
	header.Set("x-api-key", secret)
	header.Set("anthropic-version", "2023-06-01")
	resp, err := c.get(ctx, base+"/v1/models?limit=1000", header, baseURL, fetchPhases)
	if err != nil {
		slog.Debug("Anthropic /v1/models query failed", "error", httpclient.Message(err))
		return 0, false
	}
	if resp.status != http.StatusOK {
		return 0, false
	}
	payload, err := resp.value()
	if err != nil {
		slog.Debug("Anthropic /v1/models query failed", "error", err)
		return 0, false
	}
	for _, m := range payload.Get("data").Elems() {
		if m.Get("id").Text() != model {
			continue
		}
		if window, ok := jsonInt(m.Get("max_input_tokens")); ok && window > 0 {
			return window, true
		}
	}
	return 0, false
}

func (c *Catalog) portalContextLength(ctx context.Context, model, baseURL string, key auth.Credential) (int, ContextSource, bool) {
	if baseURL != "" {
		if window, ok := c.endpointContextLength(ctx, model, baseURL, key); ok {
			return window, SourcePortal, true
		}
	}
	metadata := c.openRouterModels(ctx)
	modelLower, normalized := strings.ToLower(model), strings.ToLower(normalizeModelVersion(model))
	pairs := func(id string) [2][2]string {
		bare := id
		if _, tail, ok := strings.Cut(id, "/"); ok {
			bare = tail
		}
		return [2][2]string{{strings.ToLower(bare), modelLower}, {strings.ToLower(normalizeModelVersion(bare)), normalized}}
	}
	exact := func(id string) bool {
		for _, pair := range pairs(id) {
			if pair[0] == pair[1] {
				return true
			}
		}
		return false
	}
	prefix := func(id string) bool {
		for _, pair := range pairs(id) {
			candidate, query := pair[0], pair[1]
			if strings.HasPrefix(candidate, query) && (len(candidate) == len(query) || strings.IndexByte("-:.", candidate[len(query)]) >= 0) {
				return true
			}
		}
		return false
	}
	for _, matcher := range []func(string) bool{func(id string) bool { return id == model }, exact, prefix} {
		for _, id := range metadata.IDs() {
			if !matcher(id) {
				continue
			}
			window := metadata.items[id].ContextLength
			if window == 0 {
				continue
			}
			if window <= 32768 && suggestsStale32K(id) {
				slog.Info("rejecting OpenRouter metadata for a model known to be underreported as 32K on the portal path", "context", window, "model", id)
				continue
			}
			return window, SourceOpenRouter, true
		}
	}
	return 0, "", false
}

func (c *Catalog) validateCachedContextLength(ctx context.Context, model, baseURL string, cached int, key auth.Credential) (int, bool) {
	dropped := true
	switch {
	case cached <= 0:
		slog.Warn("dropping a cache entry that is not positive, resolving again", "model", model, "base_url", shownURL(baseURL), "tokens", cached)
	case cached <= 32768 && suggestsStale32K(model):
		slog.Info("dropping a stale cached context entry known to be underreported as 32K, resolving again from the built-in table",
			"model", model, "base_url", shownURL(baseURL), "tokens", groupedInt(cached))
	case stalePreCatalogEntry(model, cached):
		slog.Info("dropping a stale cache entry written before the built-in table knew the model, resolving again",
			"model", model, "base_url", shownURL(baseURL), "tokens", groupedInt(cached))
	default:
		dropped = false
	}
	if dropped {
		c.invalidateCachedContextLength(ctx, model, baseURL)
		return 0, false
	}
	if inferred, ok := c.InferProviderFromURL(ctx, baseURL); ok && inferred == "portal" {
		slog.Debug("bypassing the persistent cache, the portal listing is authoritative", "model", model, "base_url", shownURL(baseURL))
		return 0, false
	}
	if IsLocalEndpoint(baseURL) {
		return c.reconcileLocalCachedContextLength(ctx, model, baseURL, cached, key), true
	}
	return cached, true
}

func isBedrockContext(baseURL, provider string) bool {
	return provider == "bedrock" || (baseURL != "" && strings.HasPrefix(providers.URLHostname(baseURL), "bedrock-runtime.") && providers.HostMatches(baseURL, "amazonaws.com"))
}

func (c *Catalog) bedrockProbeFailedRecently(key bedrockProbeKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, failedAt := range c.bedrockMiss {
		if now.Sub(failedAt) >= bedrockProbeFailureTTL {
			delete(c.bedrockMiss, k)
		}
	}
	_, ok := c.bedrockMiss[key]
	return ok
}

func (c *Catalog) resolveBedrockContextLength(ctx context.Context, model, baseURL string) int {
	cacheURL := baseURL
	if cacheURL == "" {
		cacheURL = bedrockCacheURL
	}
	if cached, ok := c.cachedContextLength(model, cacheURL, true); ok && cached > 0 {
		return cached
	}
	region := ""
	if m := bedrockRuntimeRegion.FindStringSubmatch(baseURL); m != nil {
		region = m[1]
	}
	if region == "" {
		region = c.bedrockRegion(ctx)
	}
	memoKey := bedrockProbeKey{baseURL: strings.TrimRight(cacheURL, "/"), model: model, region: region}
	if region != "" && !c.bedrockProbeFailedRecently(memoKey) {
		if probed, ok := c.probeBedrockContextLength(ctx, model, region); ok && probed != 0 {
			c.saveContextLength(ctx, model, cacheURL, probed, bedrockConfirmedSource)
			c.mu.Lock()
			delete(c.bedrockMiss, memoKey)
			c.mu.Unlock()
			return probed
		}
		c.mu.Lock()
		c.bedrockMiss[memoKey] = time.Now()
		c.mu.Unlock()
	}
	return c.bedrockContextLength(ctx, model, "", false)
}

func (c *Catalog) customEndpointContextLength(ctx context.Context, model, baseURL string, key auth.Credential, provider string) int {
	if window, ok := c.endpointContextLength(ctx, model, baseURL, key); ok {
		return window
	}
	if IsLocalEndpoint(baseURL) {
		if window, ok := c.probeLocalContextLength(ctx, model, baseURL, key, provider); ok {
			return window
		}
	}
	if window, ok := c.ollamaAPIShow(ctx, model, baseURL, key); ok {
		c.saveUnlessSkipped(ctx, model, baseURL, window, provider)
		return window
	}
	slog.Info("Could not detect context length, defaulting to the probe-down window. Set model.context_length in config.yaml to override.",
		"model", model, "base_url", shownURL(baseURL), "tokens", groupedInt(DefaultFallbackContext))
	if hit, ok := longestKeyMatch(defaultContextByLength, strings.ToLower(model)); ok {
		slog.Info("using the built-in context length for a custom endpoint", "tokens", groupedInt(hit.window), "model", model, "match", hit.key)
		return hit.window
	}
	c.warnContextLengthFallback(model, baseURL)
	return DefaultFallbackContext
}

func (c *Catalog) customCodexRouteContextLength(ctx context.Context, model, baseURL string, key auth.Credential, provider string) int {
	window, _, ok := c.codexOAuthContextLength(ctx, model, auth.Secret{}, "")
	token := auth.Secret{}
	if ok && window != 0 && IsCodexContextVariant(model) {
		token = key.ProbeKey(ctx)
	}
	if !token.Empty() {
		c.codexContextLengths(ctx, token, baseURL)
		if ceiling, found := c.cachedCodexCatalogMax(token, baseURL, BareCodexSlug(StripCodexContextVariantSuffix(model))); found && ceiling != 0 && ceiling < window {
			slog.Info("route catalog caps the model at its max_context_window", "base_url", shownURL(baseURL), "model", model, "tokens", groupedInt(ceiling))
			window = ceiling
		}
	}
	if ok && window != 0 {
		slog.Info("using the Codex OAuth context length on a codex_responses route", "tokens", groupedInt(window), "model", model, "base_url", shownURL(baseURL))
		return window
	}
	return c.customEndpointContextLength(ctx, model, baseURL, key, provider)
}

func (c *Catalog) moaContextLength(ctx context.Context, model string, custom []config.CustomEndpoint) (int, bool) {
	cfg, err := c.cfg.LoadReadonly(ctx)
	if err != nil {
		slog.Debug("MoA aggregator context-length resolution failed", "error", err)
		return 0, false
	}
	if custom == nil {
		loaded, err := c.cfg.CustomEndpoints(ctx)
		if err != nil {
			slog.Debug("MoA aggregator context-length resolution failed", "error", err)
			return 0, false
		}
		custom = append([]config.CustomEndpoint{}, loaded...)
	}
	name := strings.TrimSpace(model)
	if name == "" {
		name = strings.TrimSpace(cfg.MoA.DefaultPreset)
	}
	if name == "" {
		name = defaultMoAPreset
	}
	preset, ok := cfg.MoA.Presets.Get(name)
	if !ok {
		slog.Debug("MoA aggregator context-length resolution failed", "error", "the preset was not found", "preset", name)
		return 0, false
	}
	if preset.Aggregator == nil {
		return 0, false
	}
	aggProvider, aggModel := strings.TrimSpace(preset.Aggregator.Provider), strings.TrimSpace(preset.Aggregator.Model)
	if aggModel == "" || aggProvider == "" || strings.ToLower(aggProvider) == "moa" {
		return 0, false
	}
	route, err := c.routes.ResolveRoute(ctx, aggProvider, aggModel)
	if err != nil {
		slog.Debug("MoA aggregator context-length resolution failed", "error", err)
		return 0, false
	}
	provider := route.Provider
	if provider == "" {
		provider = aggProvider
	}
	return c.contextLength(ctx, ContextQuery{Model: aggModel, BaseURL: route.BaseURL, Key: route.Key, Provider: provider, Custom: custom})
}

func (c *Catalog) configOverrideContextLength(ctx context.Context, model, baseURL, provider string, custom []config.CustomEndpoint) (int, bool) {
	if provider != "" && model != "" {
		if window, ok := c.overrideContextWindow(ctx, provider, model); ok && window > 0 {
			return window, true
		}
	}
	if baseURL != "" && model != "" {
		if window, ok := c.customContextLength(ctx, model, baseURL, custom); ok && window != 0 {
			return window, true
		}
	}
	return 0, false
}

func (c *Catalog) providerAwareContextLength(ctx context.Context, model, baseURL string, key auth.Credential, provider, effective string) (int, bool) {
	if effective != "" {
		if profile, ok := c.reg.Get(ctx, effective); ok {
			if live, ok := profile.(providers.LiveContexter); ok {
				if window, ok := live.LiveContextLength(ctx, model, key.ProbeKey(ctx), baseURL); ok && window != 0 {
					return window, true
				}
			}
		}
	}
	var (
		window    int
		source    ContextSource
		persistOn ContextSource
		sourced   bool
	)
	switch effective {
	case "portal":
		window, source, _ = c.portalContextLength(ctx, model, baseURL, key)
		persistOn, sourced = SourcePortal, true
	case "openai-codex":
		window, source, _ = c.codexOAuthContextLength(ctx, model, key.ProbeKey(ctx), baseURL)
		persistOn, sourced = SourceLive, true
	}
	if sourced && window != 0 {
		if baseURL != "" && source == persistOn {
			c.saveContextLength(ctx, model, baseURL, window, "")
		}
		return window, true
	}
	if (effective == "gmi" || effective == "commandcode" || effective == "commandcode-anthropic") && baseURL != "" {
		if window, ok := c.endpointContextLength(ctx, model, baseURL, key); ok {
			return window, true
		}
	}
	if baseURL != "" {
		inferred, known := c.InferProviderFromURL(ctx, baseURL)
		if !known || strings.Contains(inferred, "ollama") {
			if window, ok := c.ollamaAPIShow(ctx, model, baseURL, key); ok {
				c.saveUnlessSkipped(ctx, model, baseURL, window, provider)
				return window, true
			}
		}
	}
	if effective == "openrouter" {
		if m, ok := c.openRouterModels(ctx).items[model]; ok && m.ContextLength > 0 && !(m.ContextLength == 32768 && suggestsKimi(model)) {
			return m.ContextLength, true
		}
	}
	if effective != "" {
		if window, ok := c.lookupModelsDevContext(ctx, effective, model, false); ok && window != 0 {
			if ModelSuggestsMiniMaxM3(model) {
				if table, _ := tableWindow("minimax-m3"); table != 0 && window < table {
					slog.Info("rejecting the models.dev context for an underreported MiniMax-M3, using the built-in window", "context", window, "model", model, "tokens", groupedInt(table))
					window = table
				}
			}
			return window, true
		}
	}
	return 0, false
}

func tableWindow(key string) (int, bool) {
	for _, row := range defaultContextLengths {
		if row.key == key {
			return row.window, true
		}
	}
	return 0, false
}

func (c *Catalog) contextLength(ctx context.Context, q ContextQuery) (window int, known bool) {
	if q.Configured > 0 {
		return q.Configured, true
	}
	if strings.ToLower(strings.TrimSpace(q.Provider)) == "moa" {
		if window, ok := c.moaContextLength(ctx, q.Model, q.Custom); ok {
			return window, true
		}
	}
	if window, ok := c.configOverrideContextLength(ctx, q.Model, q.BaseURL, q.Provider, q.Custom); ok {
		return window, true
	}
	baseURL, provider, key := q.BaseURL, q.Provider, q.Key
	if baseURL != "" && malformedURL(normalizeBaseURL(baseURL)) {
		baseURL = ""
	}
	if strings.TrimSpace(q.Model) == "" {
		slog.Info("no model id provided for context length resolution, using the default window", "tokens", groupedInt(DefaultFallbackContext))
		return DefaultFallbackContext, true
	}
	model := c.StripProviderPrefix(ctx, q.Model)
	model = c.stripOpenRouterVariant(ctx, model, baseURL, provider)
	if window, ok := endpointScopedContextLength(model, baseURL); ok {
		return window, true
	}
	if profile, ok := c.reg.Get(ctx, provider); ok {
		if limiter, ok := profile.(providers.ContextLimiter); ok {
			if window, ok := limiter.ContextLength(model); ok && window > 0 {
				return window, true
			}
		}
	}
	bedrock := isBedrockContext(baseURL, provider)
	codexRoute := c.isCodexRoute(ctx, provider, baseURL, q.Custom)
	if baseURL != "" && !bedrock && !codexRoute && !skipPersistentContextCache(provider) {
		if cached, ok := c.cachedContextLength(model, baseURL, false); ok {
			if window, ok := c.validateCachedContextLength(ctx, model, baseURL, cached, key); ok {
				return window, true
			}
		}
	}
	if bedrock {
		return c.resolveBedrockContextLength(ctx, model, baseURL), true
	}
	if provider == "novita" || (baseURL != "" && providers.HostMatches(baseURL, "api.novita.ai")) {
		target := baseURL
		if target == "" {
			target = novitaBaseURL
		}
		if window, ok := c.endpointContextLength(ctx, model, target, key); ok {
			if baseURL != "" {
				c.saveContextLength(ctx, model, baseURL, window, "")
			}
			return window, true
		}
	}
	if isCustomEndpoint(baseURL) && !c.isKnownProviderBaseURL(ctx, baseURL) && strings.ToLower(strings.TrimSpace(provider)) != "openai-codex" {
		if codexRoute {
			return c.customCodexRouteContextLength(ctx, model, baseURL, key, provider), true
		}
		return c.customEndpointContextLength(ctx, model, baseURL, key, provider), true
	}
	if provider == "anthropic" || (baseURL != "" && providers.URLHostname(baseURL) == "api.anthropic.com") {
		target := baseURL
		if target == "" {
			target = anthropicBaseURL
		}
		if window, ok := c.anthropicContextLength(ctx, model, target, key); ok && window != 0 {
			return window, true
		}
	}
	effective := provider
	if baseURL != "" && (effective == "" || effective == "openrouter" || effective == "custom") {
		if inferred, ok := c.InferProviderFromURL(ctx, baseURL); ok {
			effective = inferred
		}
	}
	if window, ok := c.providerAwareContextLength(ctx, model, baseURL, key, provider, effective); ok {
		return window, true
	}
	if effective == "" {
		if m, ok := c.openRouterModels(ctx).items[model]; ok {
			switch {
			case m.window == windowUnknown:
				return 0, false
			case m.window == windowAbsent:
				return DefaultFallbackContext, true
			case m.ContextLength == 32768 && suggestsStale32K(model):
				slog.Info("rejecting OpenRouter metadata for a model known to be underreported as 32K, using the built-in table", "context", m.ContextLength, "model", model)
			default:
				return m.ContextLength, true
			}
		}
	}
	if baseURL != "" && IsLocalEndpoint(baseURL) {
		if window, ok := c.probeLocalContextLength(ctx, model, baseURL, key, provider); ok {
			return window, true
		}
	}
	if hit, ok := longestKeyMatch(defaultContextByLength, strings.ToLower(model)); ok {
		return hit.window, true
	}
	c.warnContextLengthFallback(model, baseURL)
	return DefaultFallbackContext, true
}

func (c *Catalog) ContextLength(ctx context.Context, q ContextQuery) (window int, known bool) {
	return c.contextLength(c.scoped(ctx), q)
}
