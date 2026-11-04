package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// CatalogRows is one provider catalog answer, Fallback marks a curated outage placeholder, Native a local server listing
type CatalogRows struct {
	IDs      []string
	Fallback bool
	Native   bool
}

func plainRows(ids []string) CatalogRows { return CatalogRows{IDs: cloneStrings(ids)} }

func fallbackRows(ids []string) CatalogRows {
	return CatalogRows{IDs: cloneStrings(ids), Fallback: true}
}

// ---- plugin profile catalog hooks, registered per home ----

// ProfileCatalogHooks are the catalog seams a plugin profile may supply for one home
type ProfileCatalogHooks struct {
	// Curated ids registered as the provider's row of the static catalog
	StaticModels []string
	// Credential files whose identity folds into every cached catalog fingerprint
	CatalogCredentialFiles []string
	// The provider's own picker catalog, false falls through to the generic profile fetch
	PickerCatalog func(ctx context.Context, forceRefresh bool) ([]string, bool)
	// Live model list override, false when the fetch failed or the provider lists nothing
	FetchModels func(ctx context.Context, apiKey, baseURL string, timeout time.Duration) ([]string, bool)
	// Answer for a models probe of a base URL this profile owns, owned false means the generic HTTP probe
	ProbeModels func(ctx context.Context, baseURL, apiKey string, timeout time.Duration) (ids []string, probedURL, resolvedBaseURL string, owned bool)
	// Drops in process catalog memos on a refresh
	InvalidateCatalogCache func()
}

type profileHooksKey struct{ home, provider string }

var (
	catalogHooksMu sync.RWMutex
	catalogHooks   = map[profileHooksKey]ProfileCatalogHooks{}
)

// RegisterProfileCatalogHooks installs a plugin profile's catalog seams for one home
func RegisterProfileCatalogHooks(h, provider string, hooks ProfileCatalogHooks) {
	catalogHooksMu.Lock()
	defer catalogHooksMu.Unlock()
	catalogHooks[profileHooksKey{home.Key(h), strings.ToLower(strings.TrimSpace(provider))}] = hooks
}

// UnregisterProfileCatalogHooks removes a home's catalog seams for a provider
func UnregisterProfileCatalogHooks(h, provider string) {
	catalogHooksMu.Lock()
	defer catalogHooksMu.Unlock()
	delete(catalogHooks, profileHooksKey{home.Key(h), strings.ToLower(strings.TrimSpace(provider))})
}

func profileCatalogHooksFor(ctx context.Context, provider string) (ProfileCatalogHooks, bool) {
	catalogHooksMu.RLock()
	defer catalogHooksMu.RUnlock()
	hooks, ok := catalogHooks[profileHooksKey{home.Key(home.Dir(ctx)), strings.ToLower(strings.TrimSpace(provider))}]
	return hooks, ok
}

// Every provider with hooks registered for the bound home, in name order
func profileCatalogHookProviders(ctx context.Context) []string {
	catalogHooksMu.RLock()
	defer catalogHooksMu.RUnlock()
	key := home.Key(home.Dir(ctx))
	var out []string
	for k := range catalogHooks {
		if k.home == key {
			out = append(out, k.provider)
		}
	}
	sort.Strings(out)
	return out
}

// ---- static catalog ----

// Built in catalog rows in the order the detection ladder walks them
var staticCatalogProviderOrder = []string{
	"moa", "nous", "openai", "openai-api", "openai-codex", "xai-oauth", "gemini", "zai", "xai", "nvidia",
	"kimi-coding", "kimi-coding-cn", "stepfun", "moonshot", "minimax", "minimax-oauth", "minimax-cn", "anthropic",
	"deepseek", "xiaomi", "tencent-tokenhub", "tencent-tokenplan", "arcee", "gmi", "opencode-zen", "opencode-go",
	"kilocode", "alibaba", "alibaba-cn", "alibaba-coding-plan", "alibaba-coding-plan-cn", "alibaba-token-plan",
	"alibaba-token-plan-cn", "huggingface", "bedrock", "azure-foundry", "vertex", "novita", "ai-gateway",
}

// Built in rows in ladder order followed by any row the table gained later
func builtinStaticCatalogProviders() []string {
	present := map[string]bool{}
	for _, p := range CuratedCatalogProviders() {
		present[p] = true
	}
	out := make([]string, 0, len(present))
	seen := map[string]bool{}
	for _, p := range staticCatalogProviderOrder {
		if present[p] && !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	for _, p := range CuratedCatalogProviders() {
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

// StaticCatalog returns a provider's built in curated row else the bound home's plugin static models
func StaticCatalog(ctx context.Context, provider string) []string {
	if HasCuratedCatalog(provider) {
		return staticCatalogModels(provider)
	}
	if provider == "" {
		return []string{}
	}
	if hooks, ok := profileCatalogHooksFor(ctx, provider); ok {
		return cloneStrings(hooks.StaticModels)
	}
	return []string{}
}

// StaticCatalogProviders lists every provider with a static row in the bound home, built ins then plugin profiles
func StaticCatalogProviders(ctx context.Context) []string {
	out := builtinStaticCatalogProviders()
	seen := map[string]bool{}
	for _, p := range out {
		seen[p] = true
	}
	for _, name := range profileCatalogHookProviders(ctx) {
		hooks, _ := profileCatalogHooksFor(ctx, name)
		if len(hooks.StaticModels) > 0 && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

// GetPreferredSilentDefaultModel is the cached manifest's labeled default else the shipped constant
func GetPreferredSilentDefaultModel(ctx context.Context, provider string) string {
	if labeled := DefaultModelFromCatalogCache(ctx, provider); labeled != "" {
		return labeled
	}
	return PreferredSilentDefaultModel
}

// GetDefaultModelForProvider is the cost safe non interactive default for a configured provider without a model
func GetDefaultModelForProvider(ctx context.Context, provider string) string {
	models := staticCatalogFn(ctx, provider)
	if UsesSilentCatalogDefault(provider) {
		preferred := preferredSilentDefaultFn(ctx, provider)
		if preferred != "" && (containsString(models, preferred) || len(models) == 0) {
			return preferred
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

// True for the Claude Opus and Fable tiers, never a silent default
func isAnthropicFrontierTier(modelID string) bool {
	base := strings.ToLower(strings.SplitN(stripVendorPrefixLower(modelID), ":", 2)[0])
	if !strings.HasPrefix(base, "claude-") {
		return false
	}
	return strings.Contains(base, "opus-") || strings.Contains(base, "fable-")
}

// PickSilentDefaultModel is the labeled default when listed, else the first non frontier row, else the first row
func PickSilentDefaultModel(ctx context.Context, modelIDs []string, provider string) string {
	if len(modelIDs) == 0 {
		return ""
	}
	if preferred := preferredSilentDefaultFn(ctx, provider); containsString(modelIDs, preferred) {
		return preferred
	}
	for _, mid := range modelIDs {
		if !isAnthropicFrontierTier(mid) {
			return mid
		}
	}
	return modelIDs[0]
}

// NousDefaultModel is the model a Nous account lands on without choosing one
type NousDefaultModel struct {
	Provider string
	Model    string
	FreeTier bool
}

// RecommendedNousDefaultModel narrows the curated plus Portal list to policy and tier then picks the silent default
func RecommendedNousDefaultModel(ctx context.Context) NousDefaultModel {
	modelIDs := curatedNousModelIDsFn(ctx)
	pricing := nousPricingFn(ctx)
	if pricing == nil {
		pricing = PricingTable{}
	}
	freeTier := checkNousFreeTierFn(ctx, true, false)
	portalURL := ""
	store := NewStore(home.Dir(ctx))
	if data, err := store.Load(); err == nil {
		portalURL = stringOf(store.ProviderState(data, "nous")["portal_base_url"])
	}
	policyAllowed, _ := nousPolicyAllowedIDsFn(ctx, false)
	if freeTier {
		modelIDs, pricing = unionPortalFreeFn(ctx, modelIDs, pricing, portalURL, false)
	} else {
		modelIDs, pricing = unionPortalPaidFn(ctx, modelIDs, pricing, portalURL, false)
	}
	modelIDs = RestrictToNousPolicy(modelIDs, policyAllowed, true)
	if freeTier {
		modelIDs, _ = PartitionNousModelsByTier(modelIDs, pricing, true)
		var unsubscribed []string
		for _, mid := range modelIDs {
			if !IsSubscriptionBilled(pricing[mid]) {
				unsubscribed = append(unsubscribed, mid)
			}
		}
		if len(unsubscribed) > 0 {
			modelIDs = unsubscribed
		}
	}
	model := ""
	if !freeTier {
		model = windowPlanDefaultFn(modelIDs, PricingTableAsAny(pricing))
	}
	if model == "" {
		model = PickSilentDefaultModel(ctx, modelIDs, "nous")
	}
	return NousDefaultModel{Provider: "nous", Model: model, FreeTier: freeTier}
}

// ---- provider identity ----

// Every provider id and alias valid on the left of the provider colon model syntax
func knownProviderNames() map[string]bool {
	names := map[string]bool{"openrouter": true, "custom": true}
	for _, p := range CanonicalProviders() {
		names[p.Slug] = true
	}
	for alias := range CatalogProviderAliases() {
		names[alias] = true
	}
	return names
}

// Config loaded for the bound home, nil when unreadable
func modelsConfig(ctx context.Context) map[string]any {
	return LoadConfigSafe(home.Dir(ctx))
}

// Routable custom provider ids configured by the user
func configuredCustomProviderIDs(ctx context.Context) map[string]bool {
	ids := map[string]bool{"custom": true}
	cfg := modelsConfig(ctx)
	if cfg == nil {
		return ids
	}
	if providers, ok := asMap(cfg["providers"]); ok {
		for _, key := range sortedStringKeys(providers) {
			entry, ok := asMap(providers[key])
			if !ok {
				continue
			}
			name := stringOf(entry["name"])
			if name == "" {
				name = key
			}
			ids[CustomProviderSlug(name, key)] = true
		}
	}
	if legacy, ok := asList(cfg["custom_providers"]); ok {
		for _, raw := range legacy {
			if entry, ok := asMap(raw); ok {
				ids[CustomProviderSlug(stringOf(entry["name"]), "")] = true
			}
		}
	}
	return ids
}

// Credential presence for the listed providers table
func listedProviderHasCredentials(ctx context.Context, pid string) bool {
	switch pid {
	case "custom":
		return strings.TrimSpace(getCustomBaseURL(ctx)) != ""
	case "openrouter":
		return hasUsableSecret(GetSecretStr(ctx, "OPENROUTER_API_KEY", ""))
	}
	loggedIn, configured := providerAuthStatusFn(ctx, pid)
	return loggedIn || configured
}

// AvailableProvider is one row of the provider list shared by icebath model and the model command
type AvailableProvider struct {
	ID            string
	Label         string
	Aliases       []string
	Authenticated bool
}

// ListAvailableProviders returns id, label, aliases and credential presence per listed provider plus custom
func ListAvailableProviders(ctx context.Context) []AvailableProvider {
	aliasesFor := map[string][]string{}
	aliases := CatalogProviderAliases()
	for _, alias := range sortedStringKeys(aliases) {
		canonical := aliases[alias]
		aliasesFor[canonical] = append(aliasesFor[canonical], alias)
	}
	var slugs []string
	for _, p := range listedCanonicalProviders() {
		slugs = append(slugs, p.Slug)
	}
	slugs = append(slugs, "custom")
	out := make([]AvailableProvider, 0, len(slugs))
	for _, pid := range slugs {
		label := CatalogProviderLabel(pid)
		if label == "" {
			label = pid
		}
		row := AvailableProvider{ID: pid, Label: label, Aliases: []string{}, Authenticated: listedProviderHasCredentials(ctx, pid)}
		if list := aliasesFor[pid]; list != nil {
			row.Aliases = list
		}
		out = append(out, row)
	}
	return out
}

// ParseModelInput splits model command input into provider and model, the colon delimits only a known provider
func ParseModelInput(ctx context.Context, raw, currentProvider string, customIDs map[string]bool) (string, string) {
	stripped := strings.TrimSpace(raw)
	colon := strings.Index(stripped, ":")
	if colon > 0 {
		providerPart := strings.ToLower(strings.TrimSpace(stripped[:colon]))
		modelPart := strings.TrimSpace(stripped[colon+1:])
		if providerPart != "" && modelPart != "" && knownProviderNames()[providerPart] {
			if providerPart == "custom" {
				configured := customIDs
				if configured == nil {
					configured = configuredCustomProviderIDs(ctx)
				}
				lowered := strings.ToLower(stripped)
				var named []string
				for id := range configured {
					if id != "custom" {
						named = append(named, id)
					}
				}
				sort.SliceStable(named, func(i, j int) bool {
					if len(named[i]) != len(named[j]) {
						return len(named[i]) > len(named[j])
					}
					return named[i] < named[j]
				})
				for _, customID := range named {
					if strings.HasPrefix(lowered, strings.ToLower(customID)+":") {
						return customID, strings.TrimSpace(stripped[len(customID)+1:])
					}
				}
				if strings.Contains(modelPart, ":") {
					customName, actualModel, _ := strings.Cut(modelPart, ":")
					customName, actualModel = strings.TrimSpace(customName), strings.TrimSpace(actualModel)
					if customName != "" && actualModel != "" {
						if configured["custom:"+strings.ToLower(customName)] {
							return "custom:" + strings.ToLower(customName), actualModel
						}
						return "custom", modelPart
					}
				}
			}
			return catalogNormalizeProvider(providerPart), modelPart
		}
	}
	return currentProvider, stripped
}

// The custom endpoint model.base_url from config
func getCustomBaseURL(ctx context.Context) string {
	return strings.TrimSpace(stringOf(modelConfigDictFn(ctx)["base_url"]))
}

// The providers.<provider> block of config, empty when absent
func getProviderConfigDict(ctx context.Context, provider string) map[string]any {
	key := strings.TrimSpace(provider)
	if key == "" {
		return map[string]any{}
	}
	cfg := modelsConfig(ctx)
	if providers, ok := asMap(cfg["providers"]); ok {
		entry, ok := asMap(providers[key])
		if !ok || len(entry) == 0 {
			entry, ok = asMap(providers[strings.ToLower(key)])
		}
		if ok && entry != nil {
			return entry
		}
	}
	return map[string]any{}
}

// The main model config mapping, empty when absent
func getModelConfigDict(ctx context.Context) map[string]any {
	cfg := modelsConfig(ctx)
	if modelCfg, ok := asMap(cfg["model"]); ok && modelCfg != nil {
		return modelCfg
	}
	return map[string]any{}
}

func baseURLLooksLikeAnthropicMessages(baseURL string) bool {
	normalized := strings.TrimRight(strings.ToLower(strings.TrimSpace(baseURL)), "/")
	if normalized == "" {
		return false
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return false
	}
	path := strings.TrimRight(parsed.Path, "/")
	return strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1")
}

// Anthropic models page URL asking for the maximum page size, continuing after a cursor when given
func anthropicModelsURL(baseURL, afterID string) string {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		endpoint = "https://api.anthropic.com"
	}
	target := endpoint + "/v1/models"
	if strings.HasSuffix(endpoint, "/v1") {
		target = endpoint + "/models"
	}
	params := url.Values{}
	params.Set("limit", "1000")
	if afterID != "" {
		params.Set("after_id", afterID)
	}
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	return target + sep + params.Encode()
}

const anthropicModelsMaxPages = 20

// last_id to continue from, empty when the page is final or the server repeats a cursor
func anthropicNextCursor(page any, seenCursors map[string]bool) string {
	m, ok := asMap(page)
	if !ok || m["has_more"] != true {
		return ""
	}
	lastID, ok := m["last_id"].(string)
	if !ok || lastID == "" || seenCursors[lastID] {
		return ""
	}
	seenCursors[lastID] = true
	return lastID
}

// CuratedModelsForProvider returns picker rows from the live list when reachable else the static catalog
func CuratedModelsForProvider(ctx context.Context, provider string, forceRefresh bool) []CatalogModel {
	normalized := catalogNormalizeProvider(provider)
	if normalized == "openrouter" {
		return FetchOpenRouterModels(ctx, FetchModelsOptions{ForceRefresh: forceRefresh})
	}
	live := providerModelIDsFn(ctx, normalized, false)
	ids := live.IDs
	if len(ids) == 0 {
		ids = staticCatalogFn(ctx, normalized)
	}
	out := make([]CatalogModel, 0, len(ids))
	for _, m := range ids {
		out = append(out, CatalogModel{ID: m})
	}
	return out
}

// Raw and normalized spellings of a provider, in that order
func providerKeys(provider string) []string {
	key := strings.ToLower(strings.TrimSpace(provider))
	normalized := catalogNormalizeProvider(provider)
	var out []string
	if key != "" {
		out = append(out, key)
	}
	if normalized != "" && normalized != key {
		out = append(out, normalized)
	}
	return out
}

func keySet(keys []string) map[string]bool {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// Active picker models plus retired aliases recognized for detection
func providerCatalogNames(ctx context.Context, provider string) []string {
	return append(staticCatalogFn(ctx, provider), ProviderRetiredAliases(provider)...)
}

func modelInProviderCatalog(ctx context.Context, nameLower string, providers []string) bool {
	for _, provider := range providers {
		for _, model := range providerCatalogNames(ctx, provider) {
			if nameLower == strings.ToLower(model) {
				return true
			}
		}
	}
	return false
}

// Resolves short aliases such as sonnet or opus against static catalogs only
func resolveStaticModelAlias(ctx context.Context, nameLower string, currentKeys []string) (string, string, bool) {
	vendor, family, ok := modelAliasIdentityFn(nameLower)
	if !ok {
		return "", "", false
	}
	match := func(provider string) string {
		prefix := family
		if IsAggregatorProvider(provider) {
			prefix = vendor + "/" + family
		}
		prefix = strings.ToLower(prefix)
		for _, m := range staticCatalogFn(ctx, provider) {
			if strings.HasPrefix(strings.ToLower(m), prefix) {
				return m
			}
		}
		return ""
	}
	current := keySet(currentKeys)
	skip := map[string]bool{}
	for k := range current {
		skip[k] = true
	}
	for _, p := range AggregatorProviders() {
		skip[p] = true
	}
	for p := range borrowedModelProviders {
		skip[p] = true
	}
	candidates := append([]string{}, currentKeys...)
	for _, p := range StaticCatalogProviders(ctx) {
		if !skip[p] {
			candidates = append(candidates, p)
		}
	}
	for _, p := range AggregatorProviders() {
		if current[p] {
			candidates = append(candidates, p)
		}
	}
	for _, p := range sortedStringKeys(borrowedModelProviders) {
		if current[p] {
			candidates = append(candidates, p)
		}
	}
	for _, provider := range candidates {
		if matched := match(provider); matched != "" {
			return provider, matched, true
		}
	}
	return "", "", false
}

// DetectStaticProviderForModel auto detects a provider from static catalogs only
func DetectStaticProviderForModel(ctx context.Context, modelName, currentProvider string) (string, string, bool) {
	name := strings.TrimSpace(modelName)
	if name == "" {
		return "", "", false
	}
	nameLower := strings.ToLower(name)
	currentKeys := providerKeys(currentProvider)
	if provider, model, ok := resolveStaticModelAlias(ctx, nameLower, currentKeys); ok {
		return provider, model, true
	}
	resolvedProvider := nameLower
	if target, ok := CatalogProviderAlias(nameLower); ok {
		resolvedProvider = target
	}
	if resolvedProvider != "custom" && resolvedProvider != "openrouter" {
		defaultModels := staticCatalogFn(ctx, resolvedProvider)
		if CatalogProviderLabel(resolvedProvider) != "" && len(defaultModels) > 0 && !keySet(currentKeys)[resolvedProvider] {
			model := GetDefaultModelForProvider(ctx, resolvedProvider)
			if model == "" {
				model = defaultModels[0]
			}
			return resolvedProvider, model, true
		}
	}
	if modelInProviderCatalog(ctx, nameLower, currentKeys) {
		return "", "", false
	}
	matches := staticCatalogMatches(ctx, name, currentProvider)
	if len(matches) == 0 {
		return "", "", false
	}
	return matches[0][0], matches[0][1], true
}

// Every provider whose static catalog lists the name, in ladder order
func staticCatalogMatches(ctx context.Context, name, currentProvider string) [][2]string {
	nameLower := strings.ToLower(name)
	current := keySet(providerKeys(currentProvider))
	var out [][2]string
	if currentProvider != "custom" && !strings.HasPrefix(currentProvider, "custom:") {
		for _, pid := range StaticCatalogProviders(ctx) {
			if current[pid] || IsAggregatorProvider(pid) || borrowedModelProviders[pid] {
				continue
			}
			if modelInProviderCatalog(ctx, nameLower, []string{pid}) {
				out = append(out, [2]string{pid, name})
			}
		}
	}
	for _, pid := range sortedStringKeys(borrowedModelProviders) {
		if !current[pid] && modelInProviderCatalog(ctx, nameLower, []string{pid}) {
			out = append(out, [2]string{pid, name})
		}
	}
	return out
}

// Provider ids including custom ones from the providers config block
func configuredProviderIDs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	providers, ok := asMap(modelsConfig(ctx)["providers"])
	if !ok {
		return out
	}
	for pid := range providers {
		if key := strings.ToLower(strings.TrimSpace(pid)); key != "" {
			out[key] = true
		}
	}
	return out
}

// Routes an explicit vendor slash model prefix to a provider the user declared in config
func resolveProviderPrefix(ctx context.Context, modelName string) (string, string, bool) {
	vendor, model, ok := strings.Cut(modelName, "/")
	if !ok {
		return "", "", false
	}
	vendor, model = strings.ToLower(strings.TrimSpace(vendor)), strings.TrimSpace(model)
	if vendor == "" || model == "" {
		return "", "", false
	}
	configured := configuredProviderIDs(ctx)
	alias := vendor
	if target, ok := CatalogProviderAlias(vendor); ok {
		alias = target
	}
	for _, candidate := range []string{vendor, alias} {
		if configured[candidate] {
			return candidate, model, true
		}
	}
	return "", "", false
}

type detectionCandidate struct {
	provider string
	model    string
	stop     bool
}

// DetectProviderForModel walks the live current catalog, static catalogs, OpenRouter and configured prefixes
func DetectProviderForModel(ctx context.Context, modelName, currentProvider string) (string, string, bool) {
	name := strings.TrimSpace(modelName)
	if name == "" {
		return "", "", false
	}
	if served := currentProviderCatalogMatch(ctx, name, currentProvider); served != "" {
		if served != name {
			return currentProvider, served, true
		}
		return "", "", false
	}
	if currentProviderOwnsVendor(ctx, name, currentProvider) {
		return "", "", false
	}
	trimmed := strings.ToLower(strings.TrimSpace(currentProvider))
	noSelection := trimmed == "" || trimmed == "auto"
	var firstGuess *detectionCandidate
	named := strings.ToLower(name)
	if target, ok := CatalogProviderAlias(named); ok {
		named = target
	}
	var result *detectionCandidate
	stopped := false
	detectionCandidates(ctx, name, currentProvider, func(candidate detectionCandidate) bool {
		if candidate.stop {
			stopped = true
			return true
		}
		if candidate.provider == currentProvider || providerHasCredentialsFn(ctx, candidate.provider) || named == candidate.provider {
			c := candidate
			result = &c
			return true
		}
		if firstGuess == nil {
			c := candidate
			firstGuess = &c
		}
		modelsLog.Debug("Skipping auto-switch: no credentials configured", "model", name, "provider", candidate.provider)
		return false
	})
	if stopped {
		return "", "", false
	}
	if result != nil {
		return result.provider, result.model, true
	}
	if noSelection && firstGuess != nil {
		return firstGuess.provider, firstGuess.model, true
	}
	return resolveProviderPrefix(ctx, name)
}

// Yields provider and model guesses in ladder order until the visitor returns true, a stop entry means the current catalog owns the name
func detectionCandidates(ctx context.Context, name, currentProvider string, visit func(detectionCandidate) bool) {
	if provider, model, ok := detectStaticProviderForModelFn(ctx, name, currentProvider); ok {
		if visit(detectionCandidate{provider: provider, model: model}) {
			return
		}
		for _, sibling := range staticCatalogMatches(ctx, name, currentProvider) {
			if sibling[0] != provider || sibling[1] != model {
				if visit(detectionCandidate{provider: sibling[0], model: sibling[1]}) {
					return
				}
			}
		}
	}
	if modelInProviderCatalog(ctx, strings.ToLower(name), providerKeys(currentProvider)) {
		visit(detectionCandidate{stop: true})
		return
	}
	if orSlug := findOpenRouterSlugFn(ctx, name); orSlug != "" {
		if currentProvider == "openrouter" && orSlug == name {
			visit(detectionCandidate{stop: true})
			return
		}
		if visit(detectionCandidate{provider: "openrouter", model: orSlug}) {
			return
		}
	}
	if provider, model, ok := resolveProviderPrefix(ctx, name); ok {
		visit(detectionCandidate{provider: provider, model: model})
	}
}

// Full OpenRouter slug for a bare or partial model name
func findOpenRouterSlug(ctx context.Context, modelName string) string {
	nameLower := strings.ToLower(strings.TrimSpace(modelName))
	if nameLower == "" {
		return ""
	}
	ids := OpenRouterModelIDs(ctx, false)
	for _, mid := range ids {
		if nameLower == strings.ToLower(mid) {
			return mid
		}
	}
	for _, mid := range ids {
		if _, bare, ok := strings.Cut(mid, "/"); ok && nameLower == strings.ToLower(bare) {
			return mid
		}
	}
	return ""
}

// ProviderLabel is the human friendly label for a provider id or alias
func ProviderLabel(provider string) string {
	original := strings.TrimSpace(provider)
	if original == "" {
		original = "openrouter"
	}
	normalized := strings.ToLower(original)
	if normalized == "auto" {
		return "Auto"
	}
	normalized = catalogNormalizeProvider(normalized)
	if label := CatalogProviderLabel(normalized); label != "" {
		return label
	}
	if original == "" {
		return "OpenRouter"
	}
	return original
}

// ---- catalog merge helpers ----

// Case insensitive dedup key folded through the picker search alias table
func modelDedupKey(modelID string) string {
	return ModelAliasCanonical(strings.ToLower(strings.TrimSpace(modelID)))
}

// models.dev entries first then curated only extras, curated unchanged when models.dev is empty
func mergeWithModelsDev(ctx context.Context, provider string, curated []string) []string {
	mdev := listAgenticModelsFn(ctx, provider, true)
	if len(mdev) == 0 {
		return cloneStrings(curated)
	}
	return mergeUnique(mergeUnique(nil, mdev, nil), curated, nil)
}

// OpenAI endpoint for discovery mirroring runtime precedence
func openaiDiscoveryBaseURL(ctx context.Context, provider string) string {
	if envRaw := strings.TrimRight(strings.TrimSpace(GetSecretStr(ctx, "OPENAI_BASE_URL", "")), "/"); envRaw != "" {
		return envRaw
	}
	modelCfg := modelConfigDictFn(ctx)
	cfgProvider := strings.ToLower(strings.TrimSpace(stringOf(modelCfg["provider"])))
	sameProvider := catalogNormalizeProvider(provider) == catalogNormalizeProvider(cfgProvider)
	if (cfgProvider == "openai" || cfgProvider == "openai-api") && sameProvider {
		if cfgURL := strings.TrimRight(strings.TrimSpace(stringOf(modelCfg["base_url"])), "/"); cfgURL != "" {
			return cfgURL
		}
	}
	return "https://api.openai.com/v1"
}

// Runtime resolver over the bound home's config, secrets and credential store
func modelsResolver(ctx context.Context) *resolver {
	h := home.Dir(ctx)
	return newResolver(ctx, config.Tree(modelsConfig(ctx)), config.NewSecrets(h), h, homeCredentialSource(h))
}

// ---- per provider catalog fetchers ----

type catalogFetcher func(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool)

// Live Codex catalog for the stored principal, the curated row without a usable token
func codexPickerCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	accessToken, baseURL := "", ""
	if creds, err := resolveCodexRuntimeCredentialsReadOnlyFn(modelsPoolEnv(ctx)); err == nil {
		accessToken, baseURL = stringOf(creds["api_key"]), stringOf(creds["base_url"])
		if codexAccessTokenIsExpiring(accessToken, 0) {
			accessToken = ""
		}
	}
	ids, fallback := GetCodexModelIDs(ctx, accessToken, baseURL)
	return CatalogRows{IDs: ids, Fallback: fallback}, true
}

// Live Portal catalog for the stored credential, else the docs hosted manifest
func nousCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	env := modelsPoolEnv(withBackgroundCaller(ctx))
	if creds, err := resolveNousRuntimeCredentials(env, false, ""); err == nil && len(creds) > 0 {
		if live := fetchNousModels(ctx, stringOf(creds["api_key"]), stringOf(creds["base_url"])); len(live) > 0 {
			return plainRows(live), true
		}
	}
	if curated := GetCuratedNousModelIDs(ctx); len(curated) > 0 {
		return plainRows(curated), true
	}
	return CatalogRows{}, false
}

// Key and base URL of a simple api key provider, empty on any miss
func apiKeyCredentials(ctx context.Context, normalized string) (string, string) {
	creds, err := modelsResolver(ctx).apiKeyProviderCredentials(normalized)
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(creds.APIKey), strings.TrimSpace(creds.BaseURL)
}

// Live models listing for a simple api key provider, false on any miss
func apiKeyProviderLive(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	apiKey, baseURL := apiKeyCredentialsFn(ctx, normalized)
	if apiKey == "" || baseURL == "" {
		return CatalogRows{}, false
	}
	ids, ok := fetchAPIModelsFn(ctx, apiKey, baseURL, 0, "", nil)
	if !ok || len(ids) == 0 {
		return CatalogRows{}, false
	}
	return plainRows(ids), true
}

// Step Plan live list merged with the curated catalog
func stepfunCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	live, ok := apiKeyProviderLive(ctx, normalized, forceRefresh)
	if !ok || len(live.IDs) == 0 {
		return live, ok
	}
	curated := staticCatalogFn(ctx, normalized)
	if len(curated) == 0 {
		return live, true
	}
	return plainRows(mergeUnique(live.IDs, curated, modelDedupKey)), true
}

// Anthropic live listing merged curated first, the curated row as a placeholder when unreachable
func anthropicCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	modelCfg := modelConfigDictFn(ctx)
	cfgBaseURL, cfgAPIKey := "", ""
	if catalogNormalizeProvider(stringOf(modelCfg["provider"])) == "anthropic" {
		cfgBaseURL = strings.TrimSpace(stringOf(modelCfg["base_url"]))
		cfgAPIKey = strings.TrimSpace(stringOf(modelCfg["api_key"]))
	}
	live, ok := fetchAnthropicModels(ctx, 0, cfgBaseURL, cfgAPIKey)
	curated := staticCatalogModels("anthropic")
	if !ok || len(live) == 0 {
		return fallbackRows(curated), true
	}
	if cfgBaseURL != "" {
		return plainRows(live), true
	}
	return plainRows(mergeUnique(curated, live, nil)), true
}

// OpenAI live listing intersected with the curated agentic catalog on official hosts
func openaiCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	apiKey := strings.TrimSpace(GetSecretStr(ctx, "OPENAI_API_KEY", ""))
	if apiKey == "" {
		return CatalogRows{}, false
	}
	base := openaiDiscoveryBaseURL(ctx, normalized)
	live, ok := fetchAPIModelsFn(ctx, apiKey, base, 0, "", nil)
	if !ok || len(live) == 0 {
		return CatalogRows{}, false
	}
	if !IsOfficialOpenAIHost(base) {
		return plainRows(live), true
	}
	liveLower := map[string]bool{}
	for _, m := range live {
		liveLower[strings.ToLower(m)] = true
	}
	curated := staticCatalogFn(ctx, normalized)
	var discovered []string
	for _, m := range curated {
		if liveLower[strings.ToLower(m)] {
			discovered = append(discovered, m)
		}
	}
	for _, m := range live {
		if isAstraModel(m) {
			discovered = append(discovered, m)
		}
	}
	if len(discovered) > 0 {
		return plainRows(discovered), true
	}
	if len(curated) > 0 {
		return plainRows(curated), true
	}
	return plainRows(live), true
}

// Custom endpoint listing with the common api key env fallbacks
func customCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	baseURL := getCustomBaseURL(ctx)
	if baseURL == "" {
		return CatalogRows{}, false
	}
	modelCfg := modelConfigDictFn(ctx)
	apiKey := strings.TrimSpace(stringOf(modelCfg["api_key"]))
	for _, name := range []string{"CUSTOM_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		if apiKey != "" {
			break
		}
		apiKey = GetSecretStr(ctx, name, "")
	}
	apiMode := ""
	if baseURLLooksLikeAnthropicMessages(baseURL) {
		apiMode = string(ModeAnthropicMessages)
	}
	ids, ok := fetchAPIModelsFn(ctx, apiKey, baseURL, 0, apiMode, nil)
	if !ok || len(ids) == 0 {
		return CatalogRows{}, false
	}
	return plainRows(ids), true
}

// Live Bedrock discovery keyed by the resolved region
func bedrockCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	ids, ok := bedrockModelIDsOrNone(ctx)
	if !ok {
		return CatalogRows{}, false
	}
	return plainRows(ids), true
}

// Live models listing of the configured Azure Foundry resource with the same credential inference uses
func azureFoundryCatalog(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
	rt, err := azureFoundryRuntimeFn(ctx, normalized, modelConfigDictFn(ctx))
	if err != nil || rt == nil {
		return CatalogRows{}, false
	}
	baseURL := strings.TrimRight(strings.TrimSpace(rt.BaseURL), "/")
	if baseURL == "" || (rt.APIKey == "" && rt.TokenSource == nil) {
		return CatalogRows{}, false
	}
	ok, ids := azureProbeOpenAIModelsFn(ctx, baseURL, rt.APIKey, rt.TokenSource)
	if !ok || len(ids) == 0 {
		return CatalogRows{}, false
	}
	return plainRows(ids), true
}

// Catalog sources tried before the generic profile fetch
var providerCatalogFetchers = map[string]catalogFetcher{
	"openrouter": func(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
		return plainRows(OpenRouterModelIDs(ctx, forceRefresh)), true
	},
	"openai-codex": codexPickerCatalog,
	"nous":         nousCatalog,
	"stepfun":      stepfunCatalog,
	"gmi":          apiKeyProviderLive,
	"anthropic":    anthropicCatalog,
	"ai-gateway": func(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
		ids, ok := fetchAIGatewayModelsLive(ctx, 0)
		if !ok || len(ids) == 0 {
			return CatalogRows{}, false
		}
		return plainRows(ids), true
	},
	"deepinfra": func(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
		ids, ok := fetchDeepinfraModels(ctx, 0, forceRefresh)
		if !ok {
			return CatalogRows{IDs: []string{}}, true
		}
		return plainRows(ids), true
	},
	"ollama-cloud": func(ctx context.Context, normalized string, forceRefresh bool) (CatalogRows, bool) {
		ids, ok := fetchOllamaCloudModels(ctx, forceRefresh)
		if !ok || len(ids) == 0 {
			return CatalogRows{}, false
		}
		return plainRows(ids), true
	},
	"openai":        openaiCatalog,
	"openai-api":    openaiCatalog,
	"custom":        customCatalog,
	"bedrock":       bedrockCatalog,
	"azure-foundry": azureFoundryCatalog,
}

// Free slugs the OpenCode relay still lists but no longer serves
var opencodeFreeExcludedModels = map[string]bool{"ox-alpha-free": true, "deepseek-v4-flash-free": true, "x-preview-f-free": true}

// The built in fetcher for a provider, else its profile picker catalog hook
func catalogFetcherFor(ctx context.Context, normalized string) (catalogFetcher, bool) {
	if fetcher, ok := providerCatalogFetchers[normalized]; ok {
		return fetcher, true
	}
	hooks, ok := profileCatalogHooksFor(ctx, normalized)
	if !ok || hooks.PickerCatalog == nil {
		return nil, false
	}
	return func(ctx context.Context, _ string, forceRefresh bool) (CatalogRows, bool) {
		ids, ok := hooks.PickerCatalog(ctx, forceRefresh)
		if !ok {
			return CatalogRows{}, false
		}
		return plainRows(ids), true
	}, true
}

// Profile by canonical name, plugin registered profiles included
func modelsProfileFor(name string) (Profile, bool) {
	return ProfileFor(name)
}

// Default live models listing of a profile, bearer auth plus its default headers
func profileFetchModels(ctx context.Context, p Profile, apiKey, baseURL string, timeout time.Duration) ([]string, bool) {
	if hooks, ok := profileCatalogHooksFor(ctx, p.Name); ok && hooks.FetchModels != nil {
		return hooks.FetchModels(ctx, apiKey, baseURL, timeout)
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if !p.ModelListing {
		return nil, false
	}
	callerBase := strings.TrimSpace(baseURL)
	effectiveBase := callerBase
	if effectiveBase == "" {
		effectiveBase = p.BaseURL
	}
	customBase := callerBase != "" && strings.TrimRight(callerBase, "/") != strings.TrimRight(p.BaseURL, "/")
	target := ""
	if customBase {
		target = strings.TrimRight(callerBase, "/") + "/models"
	} else {
		target = strings.TrimSpace(p.ModelsURL)
		if target == "" {
			if effectiveBase == "" {
				return nil, false
			}
			target = strings.TrimRight(effectiveBase, "/") + "/models"
		}
	}
	headers := map[string]string{"Accept": "application/json", "User-Agent": userAgent("icebath-cli")}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	for k, v := range p.Headers {
		headers[k] = v
	}
	payload, err := modelsGetJSON(ctx, target, timeout, headers, nil)
	if err != nil {
		modelsLog.Debug("fetch_models failed", "provider", p.Name, "error", err)
		return nil, false
	}
	items := payload
	if doc, ok := asMap(payload); ok {
		items = doc["data"]
	}
	return ChatCatalogIDs(items), true
}

// Generic live fetch for any registered profile merged with its curated list
func profileLiveCatalog(ctx context.Context, normalized string) (CatalogRows, bool) {
	profile, ok := modelsProfileForFn(normalized)
	if !ok {
		return CatalogRows{}, false
	}
	if profile.AuthType == AuthTypeExternalProcess {
		live, ok := profileFetchModelsFn(ctx, profile, "", "", 0)
		if !ok {
			live = nil
		}
		return MergeProfileCatalog(ctx, normalized, profile, live)
	}
	if !(profile.AuthType == AuthTypeAPIKey && profile.BaseURL != "") {
		if len(profile.FallbackModels) > 0 {
			return plainRows(profile.FallbackModels), true
		}
		return CatalogRows{}, false
	}
	apiKey, baseURL := apiKeyCredentialsFn(ctx, normalized)
	if baseURL == "" {
		baseURL = profile.BaseURL
	}
	return ProbeProfileCatalog(ctx, normalized, profile, apiKey, baseURL)
}

// ProbeProfileCatalog fetches a profile's live list only with a key and merges it with the curated row
func ProbeProfileCatalog(ctx context.Context, normalized string, profile Profile, apiKey, baseURL string) (CatalogRows, bool) {
	var live []string
	if apiKey != "" {
		if ids, ok := profileFetchModelsFn(ctx, profile, apiKey, baseURL, 0); ok {
			live = ids
		}
	}
	return MergeProfileCatalog(ctx, normalized, profile, live)
}

// MergeProfileCatalog combines a live catalog with the curated list the way the model picker does
func MergeProfileCatalog(ctx context.Context, normalized string, profile Profile, live []string) (CatalogRows, bool) {
	var rows CatalogRows
	ok := true
	if len(live) == 0 {
		if len(profile.FallbackModels) == 0 {
			return CatalogRows{}, false
		}
		rows = fallbackRows(profile.FallbackModels)
	} else {
		curated := staticCatalogFn(ctx, normalized)
		if len(curated) == 0 {
			curated = cloneStrings(profile.FallbackModels)
		}
		if len(curated) == 0 {
			rows = plainRows(live)
		} else if IsLiveFirstPickerProvider(normalized) {
			rows = plainRows(mergeUnique(live, curated, modelDedupKey))
		} else {
			rows = plainRows(mergeUnique(curated, live, modelDedupKey))
		}
	}
	return dropDelistedOpencodeModels(normalized, rows), ok
}

// Filters retired OpenCode free slugs out of the live first Zen and Go pickers
func dropDelistedOpencodeModels(normalized string, rows CatalogRows) CatalogRows {
	if len(rows.IDs) == 0 || !IsLiveFirstPickerProvider(normalized) {
		return rows
	}
	kept := make([]string, 0, len(rows.IDs))
	for _, m := range rows.IDs {
		if !opencodeFreeExcludedModels[strings.ToLower(m)] {
			kept = append(kept, m)
		}
	}
	rows.IDs = kept
	return rows
}

// Drops generation ids keeping the row flags
func chatCatalogRows(rows CatalogRows) CatalogRows {
	rows.IDs = WithoutGenerationModels(rows.IDs)
	return rows
}

// model.base_url when it points the configured provider at a relay, empty otherwise
func configuredRelayBaseURL(ctx context.Context, provider string) string {
	modelCfg := modelConfigDictFn(ctx)
	cfgProvider := strings.ToLower(strings.TrimSpace(stringOf(modelCfg["provider"])))
	if cfgProvider == "" || provider == "" {
		return ""
	}
	normalized := catalogNormalizeProvider(provider)
	if normalized != catalogNormalizeProvider(cfgProvider) {
		return ""
	}
	baseURL := strings.TrimRight(strings.TrimSpace(stringOf(modelCfg["base_url"])), "/")
	if baseURL == "" {
		return ""
	}
	canonical := map[string]bool{}
	if profile, ok := modelsProfileForFn(normalized); ok && profile.BaseURL != "" {
		canonical[NormalizeRouteBaseURL(profile.BaseURL)] = true
	}
	if def, ok := lookupProviderDef(normalized); ok && def.BaseURL != "" {
		canonical[NormalizeRouteBaseURL(def.BaseURL)] = true
	}
	if canonical[NormalizeRouteBaseURL(baseURL)] {
		return ""
	}
	return baseURL
}

// Live catalog probed at the configured relay only, false to fall through
func relayModelCatalog(ctx context.Context, normalized, relay string) ([]string, bool) {
	profile, ok := modelsProfileForFn(normalized)
	if !ok || profile.AuthType != AuthTypeAPIKey {
		return nil, false
	}
	apiKey, _ := apiKeyCredentialsFn(ctx, normalized)
	live, ok := profileFetchModelsFn(ctx, profile, apiKey, relay, 0)
	if !ok {
		return nil, false
	}
	var out []string
	for _, m := range live {
		if m != "" {
			out = append(out, m)
		}
	}
	return out, len(out) > 0
}

// Fetchers that already resolve model.base_url themselves and degrade to curated when it fails
var relayAwareCatalogFetchers = map[string]bool{"anthropic": true, "custom": true, "openai": true, "openai-api": true, "stepfun": true, "gmi": true}

// The local no egress catalog tail, curated plus the models.dev merge where preferred
func staticCatalogTail(ctx context.Context, normalized string, hasFetcher bool) CatalogRows {
	curated := staticCatalogFn(ctx, normalized)
	rows := CatalogRows{IDs: curated, Fallback: hasFetcher}
	if !IsModelsDevPreferred(normalized) {
		return chatCatalogRows(dropDelistedOpencodeModels(normalized, rows))
	}
	rows.IDs = mergeWithModelsDevFn(ctx, normalized, curated)
	rows = dropDelistedOpencodeModels(normalized, rows)
	if normalized == "xai" || normalized == "xai-oauth" {
		rows.IDs = xaiFinalizeCatalog(rows.IDs)
	}
	return chatCatalogRows(rows)
}

// ProviderModelIDs is the best known catalog for a provider, live fetchers then profile fetch then the static list
func ProviderModelIDs(ctx context.Context, provider string, forceRefresh bool) CatalogRows {
	requested := strings.ToLower(strings.TrimSpace(provider))
	if requested == "ollama" {
		return plainRows(ollamaLocalCatalog(ctx, forceRefresh))
	}
	normalized := catalogNormalizeProvider(provider)
	relay := configuredRelayBaseURL(ctx, provider)
	fetcher, hasFetcher := catalogFetcherFor(ctx, normalized)
	if relay != "" && !relayAwareCatalogFetchers[normalized] {
		if relayed, ok := relayModelCatalogFn(ctx, normalized, relay); ok {
			return chatCatalogRows(plainRows(relayed))
		}
		return staticCatalogTail(ctx, normalized, hasFetcher)
	}
	if hasFetcher {
		if rows, ok := fetcher(ctx, normalized, forceRefresh); ok {
			return chatCatalogRows(rows)
		}
	}
	if rows, ok := profileLiveCatalogFn(ctx, normalized); ok {
		return chatCatalogRows(rows)
	}
	return staticCatalogTail(ctx, normalized, hasFetcher)
}

// ---- disk cache for provider model ids ----

const (
	providerModelsCacheTTL      = time.Hour
	providerModelsStaleServeMax = 7 * 24 * time.Hour
	providerModelsFallbackTTL   = 60 * time.Second
)

type swrKey struct{ home, cacheKey string }

var (
	swrRefreshMu       sync.Mutex
	swrRefreshInflight = map[swrKey]bool{}
	swrRefreshWait     sync.WaitGroup
)

// WaitProviderModelsRefreshes blocks until every background catalog refresh has finished
func WaitProviderModelsRefreshes() { swrRefreshWait.Wait() }

// One provider row of the disk cache
func cacheEntry(fp string, models []string, at time.Time) map[string]any {
	return map[string]any{"fp": fp, "at": float64(at.UnixNano()) / 1e9, "models": cloneStrings(models)}
}

// Row to store for a live result, false to keep the existing row over a curated placeholder
func liveResultEntry(fp string, live CatalogRows, existing any, at time.Time) (map[string]any, bool) {
	if !live.Fallback {
		return cacheEntry(fp, live.IDs, at), true
	}
	if row, ok := cacheEntryValid(existing, fp, false); ok && !truthy(row["fallback"]) {
		return nil, false
	}
	entry := cacheEntry(fp, live.IDs, at)
	entry["fallback"] = true
	return entry, true
}

// Fire and forget refresh of one cache key, at most one in flight per key and home
func spawnSWRRefresh(ctx context.Context, cacheKey string, refresh func() (map[string]any, bool)) {
	key := swrKey{"", cacheKey}
	if home.Override(ctx) != "" {
		key.home = home.Key(home.Dir(ctx))
	}
	swrRefreshMu.Lock()
	if swrRefreshInflight[key] {
		swrRefreshMu.Unlock()
		return
	}
	swrRefreshInflight[key] = true
	swrRefreshMu.Unlock()
	if refresh == nil {
		refresh = func() (map[string]any, bool) {
			live := providerModelIDsFn(ctx, cacheKey, true)
			if len(live.IDs) > 0 || (cacheKey == "ollama" && ollamaNativeProbeReachable(ctx)) {
				fp := credentialFingerprintFn(ctx, cacheKey)
				return liveResultEntry(fp, live, loadProviderModelsCacheFn(ctx)[cacheKey], modelsClock())
			}
			return nil, false
		}
	}
	swrRefreshWait.Add(1)
	go func() {
		defer swrRefreshWait.Done()
		defer func() {
			if r := recover(); r != nil {
				modelsLog.Debug("SWR refresh failed", "key", cacheKey, "panic", r)
			}
			swrRefreshMu.Lock()
			delete(swrRefreshInflight, key)
			swrRefreshMu.Unlock()
		}()
		if entry, ok := refresh(); ok && entry != nil {
			storeCacheEntry(ctx, cacheKey, entry)
		}
	}()
}

func providerModelsCachePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "provider_models_cache.json")
}

// Stat signature plus content digest of a file, the error name as the digest when unreadable
func fileCacheVersion(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	ino, ctime := fileIdentity(st)
	digest := ""
	if raw, rerr := os.ReadFile(path); rerr == nil {
		sum := sha256.Sum256(raw)
		digest = hex.EncodeToString(sum[:])
	} else {
		digest = "<" + fmt.Sprintf("%T", rerr) + ">"
	}
	return fmt.Sprintf("(%d, %d, %d, %d, '%s')", st.ModTime().UnixNano(), st.Size(), ino, ctime, digest), nil
}

// Short hash of the credentials a provider's catalog fetch would see right now
func credentialFingerprint(ctx context.Context, provider string) string {
	env := modelsPoolEnv(ctx)
	var parts []string
	if def, ok := lookupProviderDef(provider); ok {
		for _, ev := range def.KeyEnv {
			parts = append(parts, ev+"="+GetEnvPreferDotenv(env, ev))
		}
		if def.BaseURLEnv != "" {
			parts = append(parts, def.BaseURLEnv+"="+GetEnvPreferDotenv(env, def.BaseURLEnv))
		}
	}
	if profile, ok := modelsProfileForFn(provider); ok && profile.AuthType == AuthTypeExternalProcess {
		if spec, ok := externalProcessSpecFor(provider); ok {
			for _, ev := range append(cloneStrings(spec.CommandEnvVars), spec.ArgsEnvVar) {
				if ev != "" {
					parts = append(parts, ev+"="+GetEnvPreferDotenv(env, ev))
				}
			}
		}
	}
	if provider == "openai" || provider == "openai-api" {
		parts = append(parts, "effective_base="+openaiDiscoveryBaseURL(ctx, provider))
	}
	if provider == "azure-foundry" {
		parts = append(parts, "effective_base="+configBaseURLForProvider(modelConfigDictFn(ctx), "azure-foundry"))
	}
	if provider == "ollama" {
		providerCfg := getProviderConfigDict(ctx, "ollama")
		keyEnv := stringOf(providerCfg["key_env"])
		if keyEnv == "" {
			keyEnv = stringOf(providerCfg["api_key_env"])
		}
		modelCfg := modelConfigDictFn(ctx)
		baseURL := stringOf(providerCfg["base_url"])
		if baseURL == "" {
			baseURL = stringOf(providerCfg["api"])
		}
		if baseURL == "" {
			baseURL = stringOf(providerCfg["url"])
		}
		parts = append(parts,
			"OLLAMA_HOST="+GetEnvPreferDotenv(env, "OLLAMA_HOST"),
			"providers.ollama.base_url="+baseURL,
			"providers.ollama.api_key="+stringOf(providerCfg["api_key"]),
			"providers.ollama.key_env="+keyEnv,
		)
		if keyEnv != "" {
			parts = append(parts, keyEnv+"="+GetEnvPreferDotenv(env, keyEnv))
		}
		extra, _ := json.Marshal(providerCfg["extra_headers"])
		if providerCfg["extra_headers"] == nil {
			extra = []byte("{}")
		}
		parts = append(parts,
			"model.provider="+stringOf(modelCfg["provider"])+"|model.base_url="+stringOf(modelCfg["base_url"]),
			"providers.ollama.extra_headers="+string(extra),
		)
	}
	mtimePart := func(label, path string) {
		version, err := fileCacheVersion(path)
		if err == nil {
			parts = append(parts, label+"@"+version)
		} else if os.IsNotExist(err) {
			parts = append(parts, label+"@missing")
		}
	}
	if provider == "openai-codex" {
		parts = append(parts, "codex_identity="+CodexCatalogCredentialIdentity(ctx))
	} else {
		h := home.Dir(ctx)
		for _, rel := range []string{"auth.json", "credentials.json"} {
			mtimePart(rel, filepath.Join(h, rel))
		}
		var profileFiles []string
		for _, name := range profileCatalogHookProviders(ctx) {
			hooks, _ := profileCatalogHooksFor(ctx, name)
			profileFiles = append(profileFiles, hooks.CatalogCredentialFiles...)
		}
		files := append([]string{"~/.codex/auth.json", "~/.claude/.credentials.json"}, profileFiles...)
		files = append(files, "~/.minimax/credentials.json")
		for _, rel := range files {
			path := modelsExpandUser(rel)
			mtimePart(path, path)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

// Tilde expansion against the user's home directory
func modelsExpandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if userHome, err := os.UserHomeDir(); err == nil {
			return filepath.Join(userHome, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// The full cache document, empty on any error
func loadProviderModelsCache(ctx context.Context) map[string]any {
	if cache := modelsReadJSONCache(providerModelsCachePath(ctx)); cache != nil {
		return cache
	}
	return map[string]any{}
}

// Persists the cache document, silent on any error
func saveProviderModelsCache(ctx context.Context, data map[string]any) {
	if err := modelsWriteJSONCache(providerModelsCachePath(ctx), data); err != nil {
		modelsLog.Debug("provider models cache save failed", "error", err)
	}
}

// Writes one row rereading the document under the thread and cross process locks
func storeCacheEntry(ctx context.Context, cacheKey string, entry map[string]any) {
	writeUnderCacheLock(providerModelsCachePath(ctx), func() {
		cache := loadProviderModelsCacheFn(ctx)
		cache[cacheKey] = entry
		saveProviderModelsCacheFn(ctx, cache)
	})
}

// ollama stays a raw slug, everything else normalizes
func normalizedCacheSlug(provider string) string {
	requested := strings.ToLower(strings.TrimSpace(provider))
	if requested == "ollama" {
		return requested
	}
	if normalized := catalogNormalizeProvider(provider); normalized != "" {
		return normalized
	}
	return provider
}

// Astra names never confer entitlement through picker state
func modelRequiresAccountDiscovery(provider, model string) bool {
	switch normalizedCacheSlug(provider) {
	case "openai", "openai-api", "openai-codex":
		return isAstraModel(model)
	}
	return false
}

// Model id list of a cache row from either a decoded JSON array or an in process string slice
func modelsListOf(v any) ([]string, bool) {
	switch list := v.(type) {
	case []string:
		return cloneStrings(list), true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, stringOf(item))
		}
		return out, true
	}
	return nil, false
}

func entryModels(entry map[string]any) []string {
	list, _ := modelsListOf(entry["models"])
	return list
}

func dropAccountGated(provider string, models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if !modelRequiresAccountDiscovery(provider, m) {
			out = append(out, m)
		}
	}
	return out
}

// CachedModelsOptions tune one disk cached catalog read
type CachedModelsOptions struct {
	ForceRefresh bool
	TTL          time.Duration
	NonBlocking  bool
}

// CachedProviderModelIDs is the disk cached catalog, a fresh row else a live fetch persisting a non empty result
func CachedProviderModelIDs(ctx context.Context, provider string, opts CachedModelsOptions) []string {
	normalized := normalizedCacheSlug(provider)
	if normalized == "" {
		return []string{}
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = providerModelsCacheTTL
	}
	isOllama := normalized == "ollama"
	cache := loadProviderModelsCacheFn(ctx)
	fp := credentialFingerprintFn(ctx, normalized)
	entry := cache[normalized]
	now := modelsClock()
	if !opts.ForceRefresh {
		if tier := diskServeTier(entry, fp, now, isOllama, ttl); tier != "" {
			if tier == "stale" {
				spawnSWRRefreshFn(ctx, normalized, nil)
			}
			row, _ := asMap(entry)
			return WithoutGenerationModels(entryModels(row))
		}
	}
	if opts.NonBlocking && !opts.ForceRefresh {
		spawnSWRRefreshFn(ctx, normalized, nil)
		if row, ok := cacheEntryValid(entry, fp, isOllama); ok {
			return WithoutGenerationModels(dropAccountGated(normalized, entryModels(row)))
		}
		return []string{}
	}
	live := providerModelIDsFn(ctx, normalized, opts.ForceRefresh)
	if len(live.IDs) > 0 {
		fresh, ok := liveResultEntry(fp, live, entry, now)
		if !ok {
			row, _ := asMap(entry)
			return WithoutGenerationModels(dropAccountGated(normalized, entryModels(row)))
		}
		storeCacheEntry(ctx, normalized, fresh)
		return WithoutGenerationModels(cloneStrings(live.IDs))
	}
	if isOllama {
		if ollamaNativeProbeReachable(ctx) {
			storeCacheEntry(ctx, normalized, cacheEntry(fp, []string{}, now))
			return []string{}
		}
		if row, ok := asMap(entry); ok && row["fp"] == fp {
			if models, isList := modelsListOf(row["models"]); isList && len(models) > 0 {
				return WithoutGenerationModels(models)
			}
		}
		return []string{}
	}
	if row, ok := cacheEntryValid(entry, fp, false); ok {
		return WithoutGenerationModels(dropAccountGated(normalized, entryModels(row)))
	}
	return []string{}
}

// Drops every provider owned catalog memo and the Ollama native caches
func invalidateProviderCatalogMemos(ctx context.Context) {
	ClearCatalogFetchBackoff(ctx)
	clearOllamaLocalCaches()
	for _, name := range profileCatalogHookProviders(ctx) {
		if hooks, ok := profileCatalogHooksFor(ctx, name); ok && hooks.InvalidateCatalogCache != nil {
			hooks.InvalidateCatalogCache()
		}
	}
}

// ClearProviderModelsCache drops one provider's cache row under the cache lock
func ClearProviderModelsCache(ctx context.Context, provider string) {
	invalidateProviderCatalogMemos(ctx)
	path := providerModelsCachePath(ctx)
	writeUnderCacheLock(path, func() {
		cache := loadProviderModelsCacheFn(ctx)
		normalized := normalizedCacheSlug(provider)
		if _, ok := cache[normalized]; ok {
			delete(cache, normalized)
			saveProviderModelsCacheFn(ctx, cache)
		}
	})
}

// ClearAllProviderModelsCache wipes the whole cache file under the cache lock
func ClearAllProviderModelsCache(ctx context.Context) {
	invalidateProviderCatalogMemos(ctx)
	path := providerModelsCachePath(ctx)
	writeUnderCacheLock(path, func() {
		if _, err := os.Stat(path); err == nil {
			if err := os.Remove(path); err != nil {
				modelsLog.Debug("provider models cache remove failed", "error", err)
			}
		}
	})
}

// Read only api key pool credential for Anthropic discovery
func resolveAnthropicPoolCatalogCredentials(ctx context.Context) (string, string) {
	rows, err := NewStore(home.Dir(ctx)).ReadCredentialPool("anthropic")
	if err != nil {
		return "", ""
	}
	for _, entry := range rows {
		if entry["auth_type"] != AuthTypeAPIKey {
			continue
		}
		token := strings.TrimSpace(stringOf(entry["access_token"]))
		if token == "" {
			continue
		}
		baseURL := stringOf(entry["base_url"])
		if baseURL == "" {
			baseURL = stringOf(entry["inference_base_url"])
		}
		return token, strings.TrimSpace(baseURL)
	}
	return "", ""
}

// Sorted model ids from the Anthropic models endpoint, false when no credential or the fetch fails
func fetchAnthropicModels(ctx context.Context, timeout time.Duration, baseURL, apiKey string) ([]string, bool) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	resolvedBaseURL := baseURL
	token := strings.TrimSpace(apiKey)
	if token == "" {
		token = ResolveAnthropicToken(modelsPoolEnv(ctx), "")
	}
	if token == "" {
		token, resolvedBaseURL = resolveAnthropicPoolCatalogCredentials(ctx)
	}
	if token == "" {
		return nil, false
	}
	headers := map[string]string{"anthropic-version": "2023-06-01"}
	oauth := isOAuthToken(token)
	if oauth {
		headers["Authorization"] = "Bearer " + token
		headers["anthropic-beta"] = strings.Join(append(cloneStrings(anthropicCommonBetas), anthropicOAuthOnlyBetas...), ",")
	} else {
		headers["x-api-key"] = token
	}
	target := anthropicModelsURL(resolvedBaseURL, "")
	idsOf := func(page any) []string {
		doc, _ := asMap(page)
		items, _ := asList(doc["data"])
		var out []string
		for _, it := range items {
			if m, ok := asMap(it); ok && stringOf(m["id"]) != "" {
				out = append(out, stringOf(m["id"]))
			}
		}
		return out
	}
	data, err := modelsGetJSON(ctx, target, timeout, headers, nil)
	if err != nil {
		var httpErr *modelsHTTPError
		retry := false
		if oauth && errors.As(err, &httpErr) && httpErr.Status == 400 {
			body := strings.ToLower(string(httpErr.Body))
			if strings.Contains(body, "long context beta") && strings.Contains(body, "not yet available") {
				var betas []string
				for _, b := range anthropicCommonBetas {
					if b != anthropicContext1MBeta {
						betas = append(betas, b)
					}
				}
				headers["anthropic-beta"] = strings.Join(append(betas, anthropicOAuthOnlyBetas...), ",")
				data, err = modelsGetJSON(ctx, target, timeout, headers, nil)
				retry = true
			}
		}
		if !retry || err != nil {
			modelsLog.Debug("Failed to fetch Anthropic models", "error", err)
			return nil, false
		}
	}
	models := idsOf(data)
	seenCursors := map[string]bool{}
	for page := 0; page < anthropicModelsMaxPages; page++ {
		cursor := anthropicNextCursor(data, seenCursors)
		if cursor == "" {
			break
		}
		data, err = modelsGetJSON(ctx, anthropicModelsURL(resolvedBaseURL, cursor), timeout, headers, nil)
		if err != nil {
			modelsLog.Debug("Failed to fetch Anthropic models", "error", err)
			return nil, false
		}
		models = append(models, idsOf(data)...)
	}
	models = codexDedupe(models)
	tier := func(m string) int {
		switch {
		case strings.Contains(m, "opus"):
			return 0
		case strings.Contains(m, "sonnet"):
			return 1
		case strings.Contains(m, "haiku"):
			return 2
		}
		return 3
	}
	sort.SliceStable(models, func(i, j int) bool {
		ti, tj := tier(models[i]), tier(models[j])
		if ti != tj {
			return ti < tj
		}
		return models[i] < models[j]
	})
	return models, true
}

// ---- generic models endpoint probe ----

const probeNegTTL = 60 * time.Second

var (
	probeNegMu    sync.Mutex
	probeNegCache = map[string]time.Time{}
)

// host colon port of a base URL so both URL candidates share one negative entry
func probeNegKey(baseURL string) string {
	_, host, port := baseURLOrigin(baseURL)
	if host == "" {
		return ""
	}
	return host + ":" + port
}

// ProbeResult describes one models endpoint probe
type ProbeResult struct {
	Models           []string
	Found            bool
	ProbedURL        string
	ResolvedBaseURL  string
	SuggestedBaseURL string
	UsedFallback     bool
}

// ProbeAPIModels probes a models endpoint at the base then its v1 twin, anthropic mode sends x-api-key
func ProbeAPIModels(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, requestHeaders map[string]string) ProbeResult {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if normalized == "" {
		return ProbeResult{}
	}
	if owner, ok := profileForHost(normalized); ok {
		if hooks, ok := profileCatalogHooksFor(ctx, owner.Name); ok && hooks.ProbeModels != nil {
			if ids, probedURL, resolvedBase, owned := hooks.ProbeModels(ctx, normalized, apiKey, timeout); owned {
				return ProbeResult{Models: ids, Found: ids != nil, ProbedURL: probedURL, ResolvedBaseURL: resolvedBase}
			}
		}
	}
	alternateBase := normalized + "/v1"
	if strings.HasSuffix(normalized, "/v1") {
		alternateBase = strings.TrimRight(normalized[:len(normalized)-3], "/")
	}
	type candidate struct {
		base     string
		fallback bool
	}
	candidates := []candidate{{normalized, false}}
	if alternateBase != "" && alternateBase != normalized {
		candidates = append(candidates, candidate{alternateBase, true})
	}
	suggested := ""
	if alternateBase != normalized {
		suggested = alternateBase
	}
	negKey := probeNegKey(normalized)
	if negKey != "" {
		probeNegMu.Lock()
		seen, ok := probeNegCache[negKey]
		probeNegMu.Unlock()
		if ok && modelsClock().Sub(seen) < probeNegTTL {
			return ProbeResult{ProbedURL: normalized + "/models", ResolvedBaseURL: normalized, SuggestedBaseURL: suggested}
		}
	}
	headers := map[string]string{"User-Agent": userAgent("icebath-cli")}
	if parsed, err := url.Parse(normalized); err == nil && strings.EqualFold(parsed.Hostname(), "generativelanguage.googleapis.com") {
		headers["X-Goog-Api-Client"] = userAgent("icebath-agent")
	}
	if apiKey != "" && apiMode == string(ModeAnthropicMessages) {
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	} else if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	if requestHeaders != nil {
		for k, v := range NormalizeExtraHeaders(requestHeaders) {
			headers[k] = v
		}
	}
	tlsCfg, tlsErr := metadataTLSConfig(ctx, normalized)
	var tried []string
	allTimedOut := true
	for _, c := range candidates {
		target := strings.TrimRight(c.base, "/") + "/models"
		tried = append(tried, target)
		if tlsErr != nil {
			allTimedOut = false
			continue
		}
		data, err := modelsGetJSON(ctx, target, timeout, headers, tlsCfg)
		if err != nil {
			allTimedOut = allTimedOut && modelsErrIsTimeout(err)
			continue
		}
		if negKey != "" {
			probeNegMu.Lock()
			delete(probeNegCache, negKey)
			probeNegMu.Unlock()
		}
		doc, _ := asMap(data)
		items, _ := asList(doc["data"])
		probed := []string{}
		for _, item := range items {
			if m, ok := asMap(item); ok {
				if NoteCatalogItem(m) {
					continue
				}
				probed = append(probed, stringOf(m["id"]))
			} else {
				probed = append(probed, "")
			}
		}
		resolved := strings.TrimRight(c.base, "/")
		suggestedBase := normalized
		if alternateBase != c.base {
			suggestedBase = alternateBase
		}
		return ProbeResult{Models: probed, Found: true, ProbedURL: target, ResolvedBaseURL: resolved, SuggestedBaseURL: suggestedBase, UsedFallback: c.fallback}
	}
	if negKey != "" && allTimedOut {
		probeNegMu.Lock()
		probeNegCache[negKey] = modelsClock()
		probeNegMu.Unlock()
	}
	probedURL := normalized + "/models"
	if len(tried) > 0 {
		probedURL = tried[0]
	}
	return ProbeResult{ProbedURL: probedURL, ResolvedBaseURL: normalized, SuggestedBaseURL: suggested}
}

// FetchAPIModels lists the model ids of a provider's models endpoint, false when no candidate answered
func FetchAPIModels(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, headers map[string]string) ([]string, bool) {
	result := ProbeAPIModels(ctx, apiKey, baseURL, timeout, apiMode, headers)
	return result.Models, result.Found
}

// Probe credential from a literal key, a token source or a callable, empty on a mint failure
func modelsMaterializeProbeAPIKey(ctx context.Context, apiKey any) string {
	switch v := apiKey.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case *CommandTokenSource:
		if v == nil {
			return ""
		}
		token, err := v.Token(ctx)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(token)
	case func(context.Context) (string, error):
		token, err := v(ctx)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(token)
	case func() string:
		return strings.TrimSpace(v())
	}
	return ""
}

// Hash of exactly what a custom endpoint fetch is called with
func customEndpointFingerprint(apiKey any, apiMode string, headers map[string]string) string {
	identity := ""
	switch v := apiKey.(type) {
	case *CommandTokenSource:
		if v != nil {
			identity = v.CacheIdentity()
		}
	case string:
		identity = v
	}
	if headers == nil {
		headers = map[string]string{}
	}
	encoded, _ := json.Marshal(headers)
	sum := sha256.Sum256([]byte(strings.Join([]string{identity, apiMode, string(encoded)}, "|")))
	return hex.EncodeToString(sum[:8])
}

// Well formed cache row for a fingerprint, empty model lists only when the caller allows them
func cacheEntryValid(entry any, fp string, allowEmpty bool) (map[string]any, bool) {
	row, ok := asMap(entry)
	if !ok || row["fp"] != fp {
		return nil, false
	}
	models, ok := modelsListOf(row["models"])
	if !ok || (!allowEmpty && len(models) == 0) {
		return nil, false
	}
	if _, isBool := row["at"].(bool); isBool {
		return nil, false
	}
	if _, ok := floatOf(row["at"]); !ok {
		return nil, false
	}
	return row, true
}

// How the cached read serves a row without the network, fresh, stale or empty for a live fetch
func diskServeTier(entry any, fp string, now time.Time, isOllama bool, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = providerModelsCacheTTL
	}
	if isOllama && ollamaLocalModelsCacheTTL < ttl {
		ttl = ollamaLocalModelsCacheTTL
	}
	row, ok := cacheEntryValid(entry, fp, isOllama)
	if !ok {
		return ""
	}
	at, _ := floatOf(row["at"])
	age := time.Duration((float64(now.UnixNano())/1e9 - at) * float64(time.Second))
	fallback := truthy(row["fallback"])
	limit := ttl
	if fallback {
		limit = providerModelsFallbackTTL
	}
	if age < limit {
		return "fresh"
	}
	models, _ := modelsListOf(row["models"])
	if len(models) > 0 && !fallback && age < providerModelsStaleServeMax {
		return "stale"
	}
	return ""
}

// CachedFetchOptions tune one disk cached custom endpoint listing
type CachedFetchOptions struct {
	APIKey       any
	BaseURL      string
	Timeout      time.Duration
	APIMode      string
	Headers      map[string]string
	ForceRefresh bool
	CacheOnly    bool
	// Native aware discovery supplying the rows without minting a token before cache admission
	FetchModels func() (CatalogRows, bool)
	TTL         time.Duration
}

// CachedFetchAPIModels is the disk cached custom endpoint listing keyed by URL and credential fingerprint
func CachedFetchAPIModels(ctx context.Context, opts CachedFetchOptions) (CatalogRows, bool) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = providerModelsCacheTTL
	}
	live := func() (CatalogRows, bool) {
		if opts.FetchModels != nil {
			return opts.FetchModels()
		}
		ids, ok := fetchAPIModelsFn(ctx, modelsMaterializeProbeAPIKey(ctx, opts.APIKey), opts.BaseURL, timeout, opts.APIMode, opts.Headers)
		if !ok {
			return CatalogRows{}, false
		}
		return plainRows(ids), true
	}
	catalog := func(row map[string]any) CatalogRows {
		return chatCatalogRows(CatalogRows{IDs: entryModels(row), Native: row["native_catalog"] == true})
	}
	normalizedURL := strings.ToLower(strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/"))
	if normalizedURL == "" {
		if opts.CacheOnly {
			return CatalogRows{}, false
		}
		rows, ok := live()
		if !ok {
			return CatalogRows{}, false
		}
		return chatCatalogRows(rows), true
	}
	fp := customEndpointFingerprintFn(opts.APIKey, opts.APIMode, opts.Headers)
	entryOf := func(rows CatalogRows, at time.Time) map[string]any {
		entry := cacheEntry(fp, rows.IDs, at)
		entry["native_catalog"] = rows.Native
		return entry
	}
	cacheKey := "custom:" + normalizedURL + "#" + fp
	cache := loadProviderModelsCacheFn(ctx)
	entry := cache[cacheKey]
	now := modelsClock()
	entryRow, _ := asMap(entry)
	nativeRow := entryRow != nil && entryRow["native_catalog"] == true
	var row map[string]any
	valid := false
	if !opts.ForceRefresh {
		row, valid = cacheEntryValid(entry, fp, nativeRow)
	}
	if valid {
		at, _ := floatOf(row["at"])
		age := time.Duration((float64(now.UnixNano())/1e9 - at) * float64(time.Second))
		if age < ttl {
			return catalog(row), true
		}
		if len(entryModels(row)) > 0 && age < providerModelsStaleServeMax {
			spawnSWRRefreshFn(ctx, cacheKey, func() (map[string]any, bool) {
				rows, ok := live()
				if ok && (len(rows.IDs) > 0 || rows.Native) {
					return entryOf(rows, modelsClock()), true
				}
				return nil, false
			})
			return catalog(row), true
		}
	}
	if opts.CacheOnly {
		return CatalogRows{}, false
	}
	rows, ok := live()
	if ok && (len(rows.IDs) > 0 || rows.Native) {
		stored := entryOf(rows, now)
		storeCacheEntry(ctx, cacheKey, stored)
		return catalog(stored), true
	}
	if row, ok := cacheEntryValid(entry, fp, false); ok {
		return catalog(row), true
	}
	if !ok {
		return CatalogRows{}, false
	}
	return chatCatalogRows(rows), true
}

// ResetProviderModelsCaches clears the process resident probe and refresh state of this slice
func ResetProviderModelsCaches() {
	probeNegMu.Lock()
	probeNegCache = map[string]time.Time{}
	probeNegMu.Unlock()
	swrRefreshMu.Lock()
	swrRefreshInflight = map[swrKey]bool{}
	swrRefreshMu.Unlock()
}
