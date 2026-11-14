package catalog

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
)

type ProviderAvailability struct {
	ID            string
	Label         string
	Aliases       []string
	Authenticated bool
}

type FastOverride struct {
	Speed       string
	ServiceTier string
}

type detection struct {
	provider string
	model    string
	stay     bool
}

var detectSkip = []string{"", "auto", providerOpenRouter, providerCustom}

func (c *Catalog) knownProviderName(ctx context.Context, name string) bool {
	if name == providerOpenRouter || name == providerCustom {
		return true
	}
	if _, ok := c.tables(ctx).labels[name]; ok {
		return true
	}
	_, ok := c.providerAlias(ctx, name)
	return ok
}

func (c *Catalog) configuredCustomProviderIDs(ctx context.Context) []string {
	ids := []string{providerCustom}
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if cfg := c.config(ctx); cfg != nil {
		for _, key := range cfg.Providers.Keys() {
			entry, _ := cfg.Providers.Get(key)
			add(CustomProviderSlug(firstNonEmpty(entry.Name, key), key))
		}
	}
	for _, endpoint := range c.customEndpoints(ctx, nil) {
		if endpoint.Text("provider_key") == "" {
			add(CustomProviderSlug(endpoint.Text("name"), ""))
		}
	}
	return ids
}

func (c *Catalog) ParseModelInput(ctx context.Context, raw, currentProvider string, customIDs []string) (provider, model string) {
	ctx = c.scoped(ctx)
	stripped := strings.TrimSpace(raw)
	colon := strings.Index(stripped, ":")
	if colon <= 0 {
		return currentProvider, stripped
	}
	providerPart := strings.ToLower(strings.TrimSpace(stripped[:colon]))
	modelPart := strings.TrimSpace(stripped[colon+1:])
	if providerPart == "" || modelPart == "" || !c.knownProviderName(ctx, providerPart) {
		return currentProvider, stripped
	}
	if providerPart == providerCustom {
		configured := customIDs
		if configured == nil {
			configured = c.configuredCustomProviderIDs(ctx)
		}
		named := []string{}
		for _, id := range configured {
			if id != providerCustom {
				named = append(named, id)
			}
		}
		sort.SliceStable(named, func(i, j int) bool {
			if len(named[i]) != len(named[j]) {
				return len(named[i]) > len(named[j])
			}
			return named[i] < named[j]
		})
		lowered := strings.ToLower(stripped)
		for _, id := range named {
			if strings.HasPrefix(lowered, strings.ToLower(id)+":") {
				return id, strings.TrimSpace(stripped[len(id)+1:])
			}
		}
		if customName, actual, nested := strings.Cut(modelPart, ":"); nested {
			customName, actual = strings.TrimSpace(customName), strings.TrimSpace(actual)
			if customName != "" && actual != "" {
				if id := "custom:" + strings.ToLower(customName); slices.Contains(configured, id) {
					return id, actual
				}
				return providerCustom, modelPart
			}
		}
	}
	return c.normalizeProvider(ctx, providerPart), modelPart
}

func (c *Catalog) providerHasCredentials(ctx context.Context, id string) bool {
	switch id {
	case providerCustom:
		return c.customBaseURL(ctx) != ""
	case providerOpenRouter:
		present, err := providers.OpenRouterKeyPresent(ctx, c.secrets)
		return err == nil && present
	}
	status, err := c.auth.Status(ctx, id)
	return err == nil && (status.LoggedIn || status.Configured)
}

func (c *Catalog) AvailableProviders(ctx context.Context) []ProviderAvailability {
	ctx = c.scoped(ctx)
	t := c.tables(ctx)
	aliasesFor := map[string][]string{}
	for _, row := range builtinAliasRows {
		aliasesFor[row[1]] = append(aliasesFor[row[1]], row[0])
	}
	for _, alias := range t.aliasOrder {
		aliasesFor[t.aliases[alias]] = append(aliasesFor[t.aliases[alias]], alias)
	}
	ids := []string{}
	for _, entry := range c.listedProviders(ctx) {
		ids = append(ids, entry.Slug)
	}
	ids = append(ids, providerCustom)
	out := make([]ProviderAvailability, 0, len(ids))
	for _, id := range ids {
		label, ok := t.labels[id]
		if !ok {
			label = id
		}
		aliases := aliasesFor[id]
		if aliases == nil {
			aliases = []string{}
		}
		out = append(out, ProviderAvailability{ID: id, Label: label, Aliases: aliases, Authenticated: c.providerHasCredentials(ctx, id)})
	}
	return out
}

func (c *Catalog) providerKeys(ctx context.Context, provider string) []string {
	var keys []string
	for _, key := range []string{strings.ToLower(strings.TrimSpace(provider)), c.normalizeProvider(ctx, provider)} {
		if key != "" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys
}

func (c *Catalog) providerCatalogNames(ctx context.Context, provider string) []string {
	return append(slices.Clone(c.staticCatalog(ctx, provider)), retiredAliases[provider]...)
}

func (c *Catalog) modelInProviderCatalog(ctx context.Context, nameLower string, providerIDs []string) bool {
	for _, provider := range providerIDs {
		for _, model := range c.providerCatalogNames(ctx, provider) {
			if nameLower == strings.ToLower(model) {
				return true
			}
		}
	}
	return false
}

func (c *Catalog) ModelInProviderCatalog(ctx context.Context, model string, providerIDs []string) bool {
	return c.modelInProviderCatalog(c.scoped(ctx), strings.ToLower(model), providerIDs)
}

func (c *Catalog) resolveStaticModelAlias(ctx context.Context, nameLower string, currentKeys []string) (detection, bool) {
	identity, ok := ModelAlias(nameLower)
	if !ok {
		return detection{}, false
	}
	match := func(provider string) (string, bool) {
		prefix := identity.Family
		if c.isAggregatorProvider(ctx, provider) {
			prefix = identity.Vendor + "/" + identity.Family
		}
		prefix = strings.ToLower(prefix)
		for _, model := range c.staticCatalog(ctx, provider) {
			if strings.HasPrefix(strings.ToLower(model), prefix) {
				return model, true
			}
		}
		return "", false
	}
	candidates := slices.Clone(currentKeys)
	for _, provider := range c.staticCatalogProviders(ctx) {
		if !slices.Contains(currentKeys, provider) && !c.isAggregatorProvider(ctx, provider) {
			candidates = append(candidates, provider)
		}
	}
	for _, provider := range currentKeys {
		if c.isAggregatorProvider(ctx, provider) {
			candidates = append(candidates, provider)
		}
	}
	for _, provider := range candidates {
		if matched, ok := match(provider); ok {
			return detection{provider: provider, model: matched}, true
		}
	}
	return detection{}, false
}

func (c *Catalog) staticCatalogMatches(ctx context.Context, name, currentProvider string) []detection {
	var out []detection
	if currentProvider == providerCustom || strings.HasPrefix(currentProvider, "custom:") {
		return out
	}
	nameLower := strings.ToLower(name)
	currentKeys := c.providerKeys(ctx, currentProvider)
	for _, provider := range c.staticCatalogProviders(ctx) {
		if slices.Contains(currentKeys, provider) || c.isAggregatorProvider(ctx, provider) {
			continue
		}
		if c.modelInProviderCatalog(ctx, nameLower, []string{provider}) {
			out = append(out, detection{provider: provider, model: name})
		}
	}
	return out
}

func (c *Catalog) detectStaticProviderForModel(ctx context.Context, modelName, currentProvider string) (detection, bool) {
	name := strings.TrimSpace(modelName)
	if name == "" {
		return detection{}, false
	}
	nameLower := strings.ToLower(name)
	currentKeys := c.providerKeys(ctx, currentProvider)
	if matched, ok := c.resolveStaticModelAlias(ctx, nameLower, currentKeys); ok {
		return matched, true
	}
	resolved := c.aliasOrSelf(ctx, nameLower)
	if resolved != providerCustom && resolved != providerOpenRouter {
		defaults := c.staticCatalog(ctx, resolved)
		if _, labeled := c.tables(ctx).labels[resolved]; labeled && len(defaults) > 0 && !slices.Contains(currentKeys, resolved) {
			return detection{provider: resolved, model: firstNonEmpty(c.defaultModelForProvider(ctx, resolved), defaults[0])}, true
		}
	}
	if c.modelInProviderCatalog(ctx, nameLower, currentKeys) {
		return detection{}, false
	}
	if matches := c.staticCatalogMatches(ctx, name, currentProvider); len(matches) > 0 {
		return matches[0], true
	}
	return detection{}, false
}

func (c *Catalog) DetectStaticProviderForModel(ctx context.Context, modelName, currentProvider string) (provider, model string, ok bool) {
	found, ok := c.detectStaticProviderForModel(c.scoped(ctx), modelName, currentProvider)
	return found.provider, found.model, ok
}

func (c *Catalog) configuredProviderIDs(ctx context.Context) []string {
	cfg := c.config(ctx)
	if cfg == nil {
		return nil
	}
	var ids []string
	for _, key := range cfg.Providers.Keys() {
		if id := strings.ToLower(strings.TrimSpace(key)); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (c *Catalog) resolveProviderPrefix(ctx context.Context, modelName string) (detection, bool) {
	vendor, model, ok := strings.Cut(modelName, "/")
	if !ok {
		return detection{}, false
	}
	vendor, model = strings.ToLower(strings.TrimSpace(vendor)), strings.TrimSpace(model)
	if vendor == "" || model == "" {
		return detection{}, false
	}
	configured := c.configuredProviderIDs(ctx)
	for _, candidate := range []string{vendor, c.aliasOrSelf(ctx, vendor)} {
		if slices.Contains(configured, candidate) {
			return detection{provider: candidate, model: model}, true
		}
	}
	return detection{}, false
}

func (c *Catalog) findOpenRouterSlug(ctx context.Context, modelName string) (string, bool) {
	nameLower := strings.ToLower(strings.TrimSpace(modelName))
	if nameLower == "" {
		return "", false
	}
	ids := choiceIDs(c.fetchOpenRouterModels(ctx, openRouterListTimeout, false, false))
	for _, id := range ids {
		if nameLower == strings.ToLower(id) {
			return id, id != ""
		}
	}
	for _, id := range ids {
		if _, bare, ok := strings.Cut(id, "/"); ok && nameLower == strings.ToLower(bare) {
			return id, true
		}
	}
	return "", false
}

func (c *Catalog) detectionCandidates(ctx context.Context, name, currentProvider string, visit func(detection) bool) {
	if static, ok := c.detectStaticProviderForModel(ctx, name, currentProvider); ok {
		if !visit(static) {
			return
		}
		for _, sibling := range c.staticCatalogMatches(ctx, name, currentProvider) {
			if sibling != static && !visit(sibling) {
				return
			}
		}
	}
	if c.modelInProviderCatalog(ctx, strings.ToLower(name), c.providerKeys(ctx, currentProvider)) {
		visit(detection{stay: true})
		return
	}
	if slug, ok := c.findOpenRouterSlug(ctx, name); ok {
		if currentProvider == providerOpenRouter && slug == name {
			visit(detection{stay: true})
			return
		}
		if !visit(detection{provider: providerOpenRouter, model: slug}) {
			return
		}
	}
	if prefixed, ok := c.resolveProviderPrefix(ctx, name); ok {
		visit(prefixed)
	}
}

func (c *Catalog) currentProviderCatalogMatch(ctx context.Context, modelName, currentProvider string) (string, bool) {
	provider := strings.ToLower(strings.TrimSpace(currentProvider))
	if slices.Contains(detectSkip, provider) || strings.HasPrefix(provider, "custom:") || slices.Contains(detectSkip, c.normalizeProvider(ctx, provider)) {
		return "", false
	}
	wanted := strings.ToLower(strings.TrimSpace(modelName))
	if wanted == "" {
		return "", false
	}
	served, err := c.cachedProviderModelIDs(ctx, provider, false, false)
	if err != nil {
		return "", false
	}
	for _, id := range served {
		if strings.ToLower(id) == wanted {
			return id, true
		}
	}
	for _, id := range served {
		if _, bare, ok := strings.Cut(id, "/"); ok && strings.ToLower(bare) == wanted {
			return id, true
		}
	}
	return "", false
}

func (c *Catalog) currentProviderOwnsVendor(ctx context.Context, modelName, currentProvider string) bool {
	provider := strings.ToLower(strings.TrimSpace(currentProvider))
	if slices.Contains(detectSkip, provider) || strings.HasPrefix(provider, "custom:") {
		return false
	}
	normalized := c.normalizeProvider(ctx, provider)
	if slices.Contains(detectSkip, normalized) || c.isAggregatorProvider(ctx, normalized) {
		return false
	}
	vendor, ok := DetectVendor(modelName)
	if !ok {
		return false
	}
	row, _ := c.builtinStaticRow(ctx, normalized)
	if len(row) == 0 {
		return false
	}
	for _, id := range row {
		if native, ok := DetectVendor(id); !ok || native != vendor {
			return false
		}
	}
	return true
}

func (c *Catalog) switchableProvider(ctx context.Context, provider string) bool {
	id := strings.ToLower(strings.TrimSpace(provider))
	if id == "" {
		return false
	}
	if id == providerCustom || strings.HasPrefix(id, "custom:") {
		return true
	}
	if id == providerOpenRouter && auth.HasUsableSecret(c.env(ctx, "OPENROUTER_API_KEY"), 4) {
		return true
	}
	status, err := c.auth.Status(ctx, id)
	if err != nil {
		return false
	}
	if status.LoggedIn || status.Configured {
		return true
	}
	pool, err := c.auth.Pool(ctx, id)
	if err != nil || !pool.HasCredentials() {
		return false
	}
	available, err := pool.HasAvailable(ctx, "")
	return err == nil && available
}

func (c *Catalog) ProviderHasCredentials(ctx context.Context, provider string) bool {
	return c.switchableProvider(c.scoped(ctx), provider)
}

func (c *Catalog) DetectProviderForModel(ctx context.Context, modelName, currentProvider string) (provider, model string, ok bool) {
	ctx = c.scoped(ctx)
	name := strings.TrimSpace(modelName)
	if name == "" {
		return "", "", false
	}
	if served, ok := c.currentProviderCatalogMatch(ctx, name, currentProvider); ok {
		if served != name {
			return currentProvider, served, true
		}
		return "", "", false
	}
	if c.currentProviderOwnsVendor(ctx, name, currentProvider) {
		return "", "", false
	}
	current := strings.ToLower(strings.TrimSpace(currentProvider))
	noSelection := current == "" || current == "auto"
	var chosen, firstGuess *detection
	stay := false
	c.detectionCandidates(ctx, name, currentProvider, func(candidate detection) bool {
		if candidate.stay {
			stay = true
			return false
		}
		if candidate.provider == currentProvider || c.switchableProvider(ctx, candidate.provider) {
			chosen = &candidate
			return false
		}
		if c.aliasOrSelf(ctx, strings.ToLower(name)) == candidate.provider {
			chosen = &candidate
			return false
		}
		if firstGuess == nil {
			firstGuess = &candidate
		}
		slog.Debug("skipping an automatic provider switch, no credentials are configured", "model", name, "provider", candidate.provider)
		return true
	})
	switch {
	case stay:
		return "", "", false
	case chosen != nil:
		return chosen.provider, chosen.model, true
	case noSelection && firstGuess != nil:
		return firstGuess.provider, firstGuess.model, true
	}
	prefixed, ok := c.resolveProviderPrefix(ctx, name)
	return prefixed.provider, prefixed.model, ok
}

func isOpenAIFastModel(modelID string) bool {
	base, _, _ := strings.Cut(bareLower(modelID), ":")
	if base == "" || strings.Contains(base, "codex") {
		return false
	}
	return slices.ContainsFunc(openAIFastModePrefixes, func(prefix string) bool { return strings.HasPrefix(base, prefix) })
}

func ModelSupportsFastMode(modelID string) bool {
	return IsAnthropicFastModeModel(modelID) || isOpenAIFastModel(modelID) || IsGrok46Family(modelID)
}

func ModelSupportsUltrafast(modelID string) bool {
	base, _, _ := strings.Cut(bareLower(StripCodexContextVariantSuffix(modelID)), ":")
	return slices.Contains(openAIUltrafastModels, base)
}

func (c *Catalog) fastModeRouteSupported(ctx context.Context, modelID, provider, baseURL string) bool {
	allowed := map[string]string{"openai": "api.openai.com", "openai-api": "api.openai.com", "openai-codex": "chatgpt.com"}
	switch {
	case IsAnthropicFastModeModel(modelID):
		allowed = map[string]string{"anthropic": "api.anthropic.com"}
	case IsGrok46Family(modelID):
		allowed = map[string]string{"xai": "api.x.ai"}
	}
	if provider != "" {
		if _, ok := allowed[c.normalizeProvider(ctx, provider)]; !ok {
			return false
		}
	}
	host := ""
	if parsed, err := url.Parse(baseURL); err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	if host == "" {
		return true
	}
	for _, official := range allowed {
		if host == official {
			return true
		}
	}
	return false
}

func (c *Catalog) ResolveFastModeOverrides(ctx context.Context, modelID, provider, baseURL, tier string) (FastOverride, bool) {
	ctx = c.scoped(ctx)
	if !ModelSupportsFastMode(modelID) {
		return FastOverride{}, false
	}
	if (provider != "" || baseURL != "") && !c.fastModeRouteSupported(ctx, modelID, provider, baseURL) {
		return FastOverride{}, false
	}
	if tier == "ultrafast" {
		if ModelSupportsUltrafast(modelID) {
			return FastOverride{ServiceTier: "ultrafast"}, true
		}
		return FastOverride{}, false
	}
	if IsAnthropicFastModeModel(modelID) {
		return FastOverride{Speed: "fast"}, true
	}
	return FastOverride{ServiceTier: "priority"}, true
}
