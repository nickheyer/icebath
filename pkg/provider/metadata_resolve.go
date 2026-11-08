package provider

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ProfileContextLengthHook is a profile qualified context bound, zero declines
type ProfileContextLengthHook func(model string) int

// ProfileLiveContextLengthHook reads a profile's credentialed live catalog, zero declines
type ProfileLiveContextLengthHook func(ctx context.Context, model, apiKey, baseURL string) int

var (
	profileContextHooksMu     sync.RWMutex
	profileContextLengthHooks = map[string]ProfileContextLengthHook{}
	profileLiveContextHooks   = map[string]ProfileLiveContextLengthHook{}
)

// RegisterProfileContextLength installs a profile owned static context bound for one canonical provider
func RegisterProfileContextLength(providerName string, fn ProfileContextLengthHook) {
	profileContextHooksMu.Lock()
	defer profileContextHooksMu.Unlock()
	if fn == nil {
		delete(profileContextLengthHooks, NormalizeProvider(providerName))
		return
	}
	profileContextLengthHooks[NormalizeProvider(providerName)] = fn
}

// RegisterProfileLiveContextLength installs a profile owned live catalog lookup for one canonical provider
func RegisterProfileLiveContextLength(providerName string, fn ProfileLiveContextLengthHook) {
	profileContextHooksMu.Lock()
	defer profileContextHooksMu.Unlock()
	if fn == nil {
		delete(profileLiveContextHooks, NormalizeProvider(providerName))
		return
	}
	profileLiveContextHooks[NormalizeProvider(providerName)] = fn
}

func profileContextLength(providerName, model string) int {
	if _, ok := ProfileFor(providerName); !ok {
		return 0
	}
	profileContextHooksMu.RLock()
	fn := profileContextLengthHooks[NormalizeProvider(providerName)]
	if p := getProviderProfile(context.Background(), NormalizeProvider(providerName)); p != nil && p.GetModelContextLengthHook != nil {
		fn = p.GetModelContextLengthHook
	}
	profileContextHooksMu.RUnlock()
	if fn == nil {
		return 0
	}
	return fn(model)
}

func profileLiveContextLength(ctx context.Context, providerName, model, apiKey, baseURL string) int {
	if providerName == "" {
		return 0
	}
	if _, ok := ProfileFor(providerName); !ok {
		return 0
	}
	profileContextHooksMu.RLock()
	fn := profileLiveContextHooks[NormalizeProvider(providerName)]
	if p := getProviderProfile(context.Background(), NormalizeProvider(providerName)); p != nil && p.LiveContextLengthHook != nil {
		fn = p.LiveContextLengthHook
	}
	profileContextHooksMu.RUnlock()
	if fn == nil {
		return 0
	}
	return fn(ctx, model, apiKey, baseURL)
}

// MiniMax M3 on any surface
func minimaxM3Name(model string) bool {
	return strings.Contains(strings.ToLower(model), "minimax-m3")
}

func modelNameSuggestsKimi(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "kimi") || strings.Contains(lower, "moonshot")
}

// Catalog keys added after the model was reachable via a shorter catch all
var preCatalogStaleKeys = map[string]bool{
	"minimax-m3": true, "muse-spark-1.3": true, "muse-spark": true, "grok-4.3": true, "grok-4.6": true,
	"grok-4-fast": true, "grok-4.20": true, "qwen3.6-plus": true,
	"deepseek-flash": true, "deepseek-v4.1-flash": true, "deepseek-v4-flash": true, "deepseek-v4-pro": true,
	"deepseek-chat": true, "deepseek-reasoner": true,
}

// True when a persisted window is a pre catalog leftover below the specific key's value
func stalePreCatalogCacheEntry(model string, cached int) bool {
	modelLower := strings.ToLower(model)
	var matches []contextEntry
	for _, e := range defaultContextLengths {
		if catalogKeyMatches(e.key, modelLower) {
			matches = append(matches, e)
		}
	}
	if len(matches) == 0 {
		return false
	}
	specific := matches[0]
	for _, m := range matches[1:] {
		if len(m.key) > len(specific.key) {
			specific = m
		}
	}
	if !preCatalogStaleKeys[specific.key] || cached >= specific.ctx {
		return false
	}
	shorterMax, any := 0, false
	for _, m := range matches {
		if len(m.key) < len(specific.key) && (!any || m.ctx > shorterMax) {
			shorterMax, any = m.ctx, true
		}
	}
	if !any {
		shorterMax = DefaultFallbackContext
	}
	return cached <= shorterMax
}

func modelNameSuggestsMinimax(model string) bool {
	lower := strings.ToLower(model)
	return strings.HasPrefix(lower, "minimax") || strings.Contains(lower, "minimaxai/")
}

func modelNameSuggestsStale32kUnderreport(model string) bool {
	return modelNameSuggestsKimi(model) || modelNameSuggestsMinimax(model)
}

// Nous Portal context, the portal models endpoint is authoritative with OpenRouter as fallback
func resolveNousContextLength(ctx context.Context, model, baseURL, apiKey string) (int, string) {
	if baseURL != "" {
		if portal, ok := resolveEndpointContextLength(ctx, model, baseURL, apiKey); ok {
			return portal, "portal"
		}
	}
	metadata := FetchModelMetadata(ctx, false)
	safeCtx := func(orID string, entry map[string]any) (int, bool) {
		n, ok := intValue(entry["context_length"])
		if !ok {
			return 0, false
		}
		if n <= 32768 && modelNameSuggestsStale32kUnderreport(orID) {
			metadataLog.Info(fmt.Sprintf("Rejecting OpenRouter metadata context=%d for %q (known 32K underreport, Nous path); falling through to hardcoded defaults", n, orID))
			return 0, false
		}
		return n, true
	}
	modelLower, normalized := strings.ToLower(model), strings.ToLower(normalizeModelVersion(model))
	pairs := func(orID string) [][2]string {
		bare := orID
		if i := strings.Index(orID, "/"); i >= 0 {
			bare = orID[i+1:]
		}
		return [][2]string{{strings.ToLower(bare), modelLower}, {strings.ToLower(normalizeModelVersion(bare)), normalized}}
	}
	exact := func(orID string) bool {
		for _, p := range pairs(orID) {
			if p[0] == p[1] {
				return true
			}
		}
		return false
	}
	prefix := func(orID string) bool {
		for _, p := range pairs(orID) {
			candidate, query := p[0], p[1]
			if strings.HasPrefix(candidate, query) && (len(candidate) == len(query) || strings.ContainsRune("-:.", rune(candidate[len(query)]))) {
				return true
			}
		}
		return false
	}
	matchers := []func(string) bool{func(orID string) bool { return orID == model }, exact, prefix}
	for _, matcher := range matchers {
		for _, orID := range sortedStringKeys(metadata) {
			if !matcher(orID) {
				continue
			}
			if n, ok := safeCtx(orID, metadata[orID]); ok {
				return n, "openrouter"
			}
		}
	}
	return 0, ""
}

// Accepts, repairs or drops a persisted entry, zero falls through to live resolution
func validateCachedContextLength(ctx context.Context, model, baseURL string, cached int, apiKey string) int {
	type rule struct {
		hit   bool
		warn  bool
		msg   string
		shown string
	}
	rules := []rule{
		{cached <= 0, true, "Dropping non-positive cache entry %s@%s -> %s; re-resolving", fmt.Sprint(cached)},
		{cached <= 32768 && modelNameSuggestsStale32kUnderreport(model), false, "Dropping stale cached context entry %s@%s -> %s (known 32K underreport); re-resolving via hardcoded defaults", formatWithCommas(cached)},
		{stalePreCatalogCacheEntry(model, cached), false, "Dropping stale pre-catalog cache entry %s@%s -> %s; re-resolving via hardcoded defaults", formatWithCommas(cached)},
	}
	for _, r := range rules {
		if !r.hit {
			continue
		}
		if r.warn {
			metadataLog.Warn(fmt.Sprintf(r.msg, model, baseURL, r.shown))
		} else {
			metadataLog.Info(fmt.Sprintf(r.msg, model, baseURL, r.shown))
		}
		invalidateCachedContextLength(ctx, model, baseURL)
		return 0
	}
	if inferProviderFromBaseURL(baseURL) == "nous" {
		metadataLog.Debug("bypassing persistent cache, Nous portal authoritative", "model", model, "base_url", baseURL)
		return 0
	}
	if IsLocalEndpoint(baseURL) {
		return reconcileLocalCachedContextLength(ctx, model, baseURL, cached, apiKey)
	}
	return cached
}

// Prunes expired Bedrock probe failures and reports the cooldown of this key
func bedrockProbeFailedRecently(key bedrockProbeKey) bool {
	now := monotonicNow()
	bedrockProbeFailureMu.Lock()
	defer bedrockProbeFailureMu.Unlock()
	for k, failedAt := range bedrockProbeFailureCache {
		if now.Sub(failedAt) >= bedrockProbeFailureTTL {
			delete(bedrockProbeFailureCache, k)
		}
	}
	_, ok := bedrockProbeFailureCache[key]
	return ok
}

func isBedrockContext(baseURL, providerName string) bool {
	if providerName == "bedrock" {
		return true
	}
	return baseURL != "" && strings.HasPrefix(Hostname(baseURL), "bedrock-runtime.") && HostMatches(baseURL, "amazonaws.com")
}

var bedrockRegionRe = regexp.MustCompile(`bedrock-runtime\.([a-z0-9-]+)\.`)

// Bedrock confirmed cache then one cooled down live probe, else the static table for this call only
func resolveBedrockContextLength(ctx context.Context, model, baseURL string) int {
	cacheKeyURL := baseURL
	if cacheKeyURL == "" {
		cacheKeyURL = "bedrock://"
	}
	if cached, ok := GetCachedContextLength(ctx, model, cacheKeyURL, true); ok && cached > 0 {
		return cached
	}
	region := ""
	if baseURL != "" {
		if m := bedrockRegionRe.FindStringSubmatch(baseURL); m != nil {
			region = m[1]
		}
	}
	if region == "" {
		cfg := metadataConfig(ctx)
		region = resolveBedrockRuntimeRegion(map[string]any{"bedrock": cfg["bedrock"]})
	}
	memoKey := bedrockProbeKey{metadataHome(ctx), strings.TrimRight(cacheKeyURL, "/"), model, region}
	if region != "" && !bedrockProbeFailedRecently(memoKey) {
		if probed := probeBedrockContextLength(ctx, model, region); probed > 0 {
			SaveProviderContextLength(ctx, model, cacheKeyURL, probed, "bedrock")
			bedrockProbeFailureMu.Lock()
			delete(bedrockProbeFailureCache, memoKey)
			bedrockProbeFailureMu.Unlock()
			return probed
		}
		bedrockProbeFailureMu.Lock()
		bedrockProbeFailureCache[memoKey] = monotonicNow()
		bedrockProbeFailureMu.Unlock()
	}
	return bedrockStaticContextLength(model)
}

// Custom endpoint resolution through models, local probes, Ollama show, catalog and default
func resolveCustomEndpointContextLength(ctx context.Context, model, baseURL, apiKey, providerName string) int {
	if n, ok := resolveEndpointContextLength(ctx, model, baseURL, apiKey); ok {
		return n
	}
	if IsLocalEndpoint(baseURL) {
		if local := probeLocalContextLength(ctx, model, baseURL, apiKey, providerName); local > 0 {
			return local
		}
	}
	if n := queryOllamaAPIShow(ctx, model, baseURL, apiKey); n > 0 {
		saveUnlessSkipped(ctx, model, baseURL, n, providerName)
		return n
	}
	metadataLog.Info(fmt.Sprintf("Could not detect context length for model %q at %s — defaulting to %s tokens (probe-down). Set model.context_length in config.yaml to override.", model, baseURL, formatWithCommas(DefaultFallbackContext)))
	if hit, ok := longestKeyMatch(defaultContextLengths, strings.ToLower(model)); ok {
		metadataLog.Info(fmt.Sprintf("Using hardcoded context length %s for model %q (custom endpoint, catalog match on %q)", formatWithCommas(hit.ctx), model, hit.key))
		return hit.ctx
	}
	warnContextLengthFallback(model, baseURL)
	return DefaultFallbackContext
}

// Codex proxy on a generic url, the Codex OAuth table answers before the endpoint probes
func resolveCustomCodexRouteContextLength(ctx context.Context, model, baseURL, apiKey, providerName string) int {
	n, _ := resolveCodexOAuthContextLengthWithSource(ctx, model, "", "")
	if n > 0 && apiKey != "" && IsCodexContextVariant(model) {
		fetchCodexOAuthContextLengthsWithSource(ctx, apiKey, baseURL)
		if cap := cachedCodexCatalogMax(apiKey, baseURL, bareCodexSlug(StripCodexContextVariantSuffix(model))); cap > 0 && cap < n {
			metadataLog.Info(fmt.Sprintf("Route catalog at %s caps %q at max_context_window %s", baseURL, model, formatWithCommas(cap)))
			n = cap
		}
	}
	if n > 0 {
		metadataLog.Info(fmt.Sprintf("Using Codex OAuth context length %s for model %q (codex_responses route at %s)", formatWithCommas(n), model, baseURL))
		return n
	}
	return resolveCustomEndpointContextLength(ctx, model, baseURL, apiKey, providerName)
}

// MoA virtual provider, resolves the aggregator's real provider and model
func resolveMoaContextLength(ctx context.Context, model string, customProviders []map[string]any) int {
	cfg := metadataConfig(ctx)
	if cfg == nil {
		metadataLog.Debug("MoA aggregator context-length resolution failed", "reason", "config unreadable")
		return 0
	}
	if customProviders == nil {
		customProviders = CompatibleCustomProviders(cfg)
	}
	aggProvider, aggModel, ok := resolveMoaAggregator(config.Tree(cfg), model)
	aggProvider, aggModel = strings.TrimSpace(aggProvider), strings.TrimSpace(aggModel)
	if !ok || aggModel == "" || aggProvider == "" || strings.ToLower(aggProvider) == "moa" {
		return 0
	}
	h := metadataHome(ctx)
	rt, err := newResolver(ctx, config.Tree(cfg), config.NewSecrets(h), h, homeCredentialSource(h)).ResolveRuntime(aggProvider, "", "", nil, aggModel)
	if err != nil || rt == nil {
		metadataLog.Debug("MoA aggregator context-length resolution failed", "error", err)
		return 0
	}
	providerName := rt.Provider
	if providerName == "" {
		providerName = aggProvider
	}
	return GetModelContextLength(ctx, ContextLengthRequest{Model: aggModel, BaseURL: rt.BaseURL, APIKey: rt.APIKey, Provider: providerName, CustomProviders: customProviders})
}

// Config only overrides, explicit model_overrides then custom_providers per model context
func configOverrideContextLength(ctx context.Context, model, baseURL, providerName string, customProviders []map[string]any) int {
	if providerName != "" && model != "" {
		if n, ok := modelsDevOverrideContextWindow(ctx, providerName, model); ok && n > 0 {
			return n
		}
	}
	if baseURL != "" && model != "" {
		if cfg := metadataConfig(ctx); cfg != nil {
			if n := CustomProviderContextLength(model, baseURL, customProviders, cfg); n > 0 {
				return n
			}
		}
	}
	return 0
}

// Provider specific sources in order, zero when all miss
func resolveProviderAwareContextLength(ctx context.Context, model, baseURL, apiKey, providerName, effectiveProvider string) int {
	if n := profileLiveContextLength(ctx, effectiveProvider, model, apiKey, baseURL); n > 0 {
		return n
	}
	switch effectiveProvider {
	case "nous":
		n, source := resolveNousContextLength(ctx, model, baseURL, apiKey)
		if n > 0 {
			if baseURL != "" && source == "portal" {
				SaveContextLength(ctx, model, baseURL, n, "")
			}
			return n
		}
	case "openai-codex":
		n, source := resolveCodexOAuthContextLengthWithSource(ctx, model, apiKey, baseURL)
		if n > 0 {
			if baseURL != "" && source == "live" {
				SaveContextLength(ctx, model, baseURL, n, "")
			}
			return n
		}
	}
	if (effectiveProvider == "gmi" || effectiveProvider == "commandcode" || effectiveProvider == "commandcode-anthropic") && baseURL != "" {
		if n, ok := resolveEndpointContextLength(ctx, model, baseURL, apiKey); ok {
			return n
		}
	}
	if baseURL != "" {
		inferred := inferProviderFromBaseURL(baseURL)
		if inferred == "" || strings.Contains(inferred, "ollama") {
			if n := queryOllamaAPIShow(ctx, model, baseURL, apiKey); n > 0 {
				saveUnlessSkipped(ctx, model, baseURL, n, providerName)
				return n
			}
		}
	}
	if effectiveProvider == "openrouter" {
		if entry, ok := FetchModelMetadata(ctx, false)[model]; ok {
			if orCtx, ok := intValue(entry["context_length"]); ok && orCtx > 0 && !(orCtx == 32768 && modelNameSuggestsKimi(model)) {
				return orCtx
			}
		}
	}
	if effectiveProvider != "" {
		if n := lookupModelsDevContext(ctx, effectiveProvider, model); n > 0 {
			if minimaxM3Name(model) {
				catalog := DefaultContextLengths()["minimax-m3"]
				if catalog > 0 && n < catalog {
					metadataLog.Info(fmt.Sprintf("Rejecting models.dev context=%d for %q (MiniMax-M3 underreport); using hardcoded default %s", n, model, formatWithCommas(catalog)))
					n = catalog
				}
			}
			return n
		}
	}
	return 0
}

// ContextLengthRequest names the route a context length is resolved for
type ContextLengthRequest struct {
	Model               string
	BaseURL             string
	APIKey              string
	Provider            string
	ConfigContextLength int
	CustomProviders     []map[string]any
}

// GetModelContextLength resolves the window through config, caches, probes, catalogs and defaults
func GetModelContextLength(ctx context.Context, req ContextLengthRequest) int {
	if ctx == nil {
		ctx = context.Background()
	}
	model, baseURL, apiKey, providerName := req.Model, req.BaseURL, req.APIKey, req.Provider
	if req.ConfigContextLength > 0 {
		return req.ConfigContextLength
	}
	if strings.ToLower(strings.TrimSpace(providerName)) == "moa" {
		if n := resolveMoaContextLength(ctx, model, req.CustomProviders); n > 0 {
			return n
		}
	}
	if n := configOverrideContextLength(ctx, model, baseURL, providerName, req.CustomProviders); n > 0 {
		return n
	}
	if baseURL != "" {
		if parsed, err := url.Parse(normalizeBaseURL(baseURL)); err != nil || (parsed.Port() != "" && !allDigits(parsed.Port())) {
			baseURL = ""
		}
	}
	if strings.TrimSpace(model) == "" {
		metadataLog.Info(fmt.Sprintf("No model id provided for context length resolution — defaulting to %s tokens.", formatWithCommas(DefaultFallbackContext)))
		return DefaultFallbackContext
	}
	model = stripProviderPrefix(model)
	model = stripOpenRouterRoutingVariant(model, baseURL, providerName)
	if n, ok := endpointScopedContextLength(model, baseURL); ok {
		return n
	}
	if n := profileContextLength(providerName, model); n > 0 {
		return n
	}
	bedrock := isBedrockContext(baseURL, providerName)
	codexRoute := isCodexRoute(ctx, providerName, baseURL, req.CustomProviders)
	if baseURL != "" && !bedrock && !codexRoute && !skipPersistentContextCache(providerName) {
		if cached, ok := GetCachedContextLength(ctx, model, baseURL, false); ok {
			if validated := validateCachedContextLength(ctx, model, baseURL, cached, apiKey); validated > 0 {
				return validated
			}
		}
	}
	if bedrock {
		if n := resolveBedrockContextLength(ctx, model, baseURL); n > 0 {
			return n
		}
	}
	if providerName == "novita" || (baseURL != "" && HostMatches(baseURL, "api.novita.ai")) {
		target := baseURL
		if target == "" {
			target = "https://api.novita.ai/openai/v1"
		}
		if n, ok := resolveEndpointContextLength(ctx, model, target, apiKey); ok {
			if baseURL != "" {
				SaveContextLength(ctx, model, baseURL, n, "")
			}
			return n
		}
	}
	if isCustomEndpoint(baseURL) && !isKnownProviderBaseURL(baseURL) && strings.ToLower(strings.TrimSpace(providerName)) != "openai-codex" {
		if codexRoute {
			return resolveCustomCodexRouteContextLength(ctx, model, baseURL, apiKey, providerName)
		}
		return resolveCustomEndpointContextLength(ctx, model, baseURL, apiKey, providerName)
	}
	if providerName == "anthropic" || (baseURL != "" && Hostname(baseURL) == "api.anthropic.com") {
		target := baseURL
		if target == "" {
			target = "https://api.anthropic.com"
		}
		if n := queryAnthropicContextLength(ctx, model, target, apiKey); n > 0 {
			return n
		}
	}
	effective := providerName
	if baseURL != "" && (effective == "" || effective == "openrouter" || effective == "custom") {
		if inferred := inferProviderFromBaseURL(baseURL); inferred != "" {
			effective = inferred
		}
	}
	if n := resolveProviderAwareContextLength(ctx, model, baseURL, apiKey, providerName, effective); n > 0 {
		return n
	}
	if effective == "" {
		if entry, ok := FetchModelMetadata(ctx, false)[model]; ok {
			orCtx := intOrDefault(entry["context_length"], DefaultFallbackContext)
			if orCtx == 32768 && modelNameSuggestsStale32kUnderreport(model) {
				metadataLog.Info(fmt.Sprintf("Rejecting OpenRouter metadata context=%d for %q (known 32K underreport); falling through to hardcoded defaults", orCtx, model))
			} else {
				return orCtx
			}
		}
	}
	if baseURL != "" && IsLocalEndpoint(baseURL) {
		if n := probeLocalContextLength(ctx, model, baseURL, apiKey, providerName); n > 0 {
			return n
		}
	}
	if hit, ok := longestKeyMatch(defaultContextLengths, strings.ToLower(model)); ok {
		return hit.ctx
	}
	warnContextLengthFallback(model, baseURL)
	return DefaultFallbackContext
}

// Context window the tool search gate sizes against, the full resolver over the routed provider credentials
func ToolModelContextLength(ctx context.Context, q tools.ContextLengthQuery) int {
	return GetModelContextLength(ctx, ContextLengthRequest{Model: q.Model, BaseURL: q.BaseURL, APIKey: q.APIKey,
		Provider: q.Provider, ConfigContextLength: q.ConfigContextLength})
}

// Context window of a model on a base url with the key the route carries
func modelContextLength(ctx context.Context, model, baseURL, apiKey string) int {
	return GetModelContextLength(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL, APIKey: apiKey})
}
