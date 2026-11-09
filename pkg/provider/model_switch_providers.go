package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Rows never capped by max models, aggregators with full catalogs and rows already curated
var uncappedPickerProviders = map[string]bool{"opencode-zen": true, "opencode-go": true, "nous": true, "openrouter": true}

// AuthenticatedProvidersOptions mirrors the list_authenticated_providers keyword arguments
type AuthenticatedProvidersOptions struct {
	CurrentProvider            string
	CurrentBaseURL             string
	CurrentModel               string
	UserProviders              map[string]any
	CustomProviders            []any
	ForceFreshNousTier         bool
	MaxModels                  *int
	Refresh                    bool
	SkipCustomProviderProbe    bool
	ProbeCurrentCustomProvider bool
	ForPicker                  bool
	ExcludedProviders          []string
	NonBlockingCatalogs        bool
	FastCustomProbe            *bool
}

// PickerProvidersOptions mirrors the list_picker_providers keyword arguments
type PickerProvidersOptions struct {
	CurrentProvider            string
	CurrentBaseURL             string
	UserProviders              map[string]any
	CustomProviders            []any
	MaxModels                  *int
	CurrentModel               string
	IncludeMoA                 bool
	ExcludedProviders          []string
	NonBlockingCatalogs        bool
	SkipCustomProviderProbe    bool
	ProbeCurrentCustomProvider bool
}

// Seams the picker tests replace
var (
	pickerAWSCredentialsFn    = hasAWSCredentials
	pickerVertexCredentialsFn = func(ctx context.Context) bool { return hasVertexCredentials(ctx, vertexHomeEnv(ctx)) }
	pickerPrefetchFn          = prefetchProviderModelsParallel
	pickerFetchLiveModelsFn   = fetchPickerLiveModels
	pickerFetchModelsDevFn    = FetchModelsDev
	pickerOllamaCloudFn       = FetchOllamaCloudModels
	pickerLMStudioFn          = FetchLMStudioModels
	pickerDiscoverEndpointFn  = discoverEndpointModels
	pickerSaveDiscoveredFn    = saveDiscoveredModelsToConfig
	pickerNousModelIDsFn      = nousPickerModelIDs
	pickerListAuthenticatedFn = ListAuthenticatedProviders
	pickerOpenRouterModelsFn  = FetchOpenRouterModels
	pickerCanonicalFn         = CanonicalProviders
	pickerProviderInfoFn      = ModelsDevProviderInfo
	pickerNousPricingFn       = func(ctx context.Context) PricingTable { return GetPricingForProvider(ctx, "nous", "", false, true) }
	pickerPrewarmDone         atomic.Bool
	pickerLoadPoolFn          = func(ctx context.Context, provider string) pickerPool {
		if pool := LoadPool(modelsPoolEnv(ctx), provider); pool != nil {
			return pool
		}
		return nil
	}
)

// Credential pool view the picker gates on
type pickerPool interface {
	HasCredentials() bool
	HasAvailable(model string) bool
}

// Writes a successful live probe into every matching custom_providers entry whose catalog is stale
func saveDiscoveredModelsToConfig(ctx context.Context, apiURL string, modelIDs []string, apiMode string, headers map[string]string, credentialIdentity *string) {
	if apiURL == "" || len(modelIDs) == 0 {
		return
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return
	}
	cfg, _ := deepCopyAny(map[string]any(tree)).(map[string]any)
	providers, ok := cfg["custom_providers"].([]any)
	if !ok {
		return
	}
	normURL := strings.ToLower(strings.TrimRight(strings.TrimSpace(apiURL), "/"))
	changed := false
	for _, item := range providers {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rawURL := entry["base_url"]
		if !pyTruthy(rawURL) {
			rawURL = entry["url"]
		}
		entryURL := ""
		if pyTruthy(rawURL) {
			entryURL = strings.TrimSpace(pyStr(rawURL))
		}
		if strings.ToLower(strings.TrimRight(entryURL, "/")) != normURL || entryAPIMode(entry) != apiMode {
			continue
		}
		if headers != nil && !stringMapsEqual(extraHeadersFromConfig(entry), headers) {
			continue
		}
		if credentialIdentity != nil {
			if _, _, identity := entryCredentials(ctx, entry, "key_env", "api_key_env"); identity != *credentialIdentity {
				continue
			}
		}
		if !discoveredCatalogStale(entry, modelIDs) {
			continue
		}
		models := make(map[string]any, len(modelIDs))
		for _, id := range modelIDs {
			models[id] = map[string]any{}
		}
		entry["models"] = models
		entry["models_discovered"] = true
		changed = true
	}
	if changed {
		cfg["custom_providers"] = providers
		_ = config.SaveConfig(ctx, h, cfg, config.SaveOptions{})
	}
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// A discovered mapping may be refreshed when stale, a curated mapping or list of dicts never is
func discoveredCatalogStale(entry map[string]any, modelIDs []string) bool {
	existing := entry["models"]
	existingMap, isMap := existing.(map[string]any)
	legacy := isMap && existingMap["__discovered_model_catalog__"] == true
	discovered := entry["models_discovered"] == true || legacy
	if isMap {
		return discovered && (legacy || !sameStringSet(sortedStringKeys(existingMap), modelIDs))
	}
	if list, ok := existing.([]any); ok {
		for _, m := range list {
			if _, isDict := m.(map[string]any); isDict {
				return false
			}
		}
		if len(list) != len(modelIDs) {
			return true
		}
		for i, m := range list {
			if s, ok := m.(string); !ok || s != modelIDs[i] {
				return true
			}
		}
		return false
	}
	return true
}

// Config mappings carry no key order, so a stored catalog matches the probe as a set
func sameStringSet(sortedKeys, ids []string) bool {
	if len(sortedKeys) != len(ids) {
		return false
	}
	other := append([]string(nil), ids...)
	sort.Strings(other)
	for i := range other {
		if other[i] != sortedKeys[i] {
			return false
		}
	}
	return true
}

// Native aware picker fetch through the shared catalog cache, Native set for an Ollama native catalog
func fetchPickerLiveModels(ctx context.Context, apiKey any, apiURL, nativeProvider string, preserveNative bool, headers map[string]string, timeout time.Duration, apiMode string, cache bool) (CatalogRows, bool) {
	switch apiKey.(type) {
	case *CommandTokenSource, func(context.Context) (string, error):
		return CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: apiKey, BaseURL: apiURL, Timeout: timeout, Headers: headers, APIMode: apiMode,
			FetchModels: func() (CatalogRows, bool) {
				return fetchPickerLiveModels(ctx, modelsMaterializeProbeAPIKey(ctx, apiKey), apiURL, nativeProvider, preserveNative, headers, timeout, apiMode, false)
			}})
	}
	key := modelsMaterializeProbeAPIKey(ctx, apiKey)
	candidate := map[string]string{}
	for k, v := range getOllamaNativeHeadersFn(ctx, apiURL, key) {
		candidate[k] = v
	}
	drop := func(pred func(string) bool) {
		for k := range candidate {
			if pred(strings.ToLower(k)) {
				delete(candidate, k)
			}
		}
	}
	callerHasAuthorization := false
	for k := range headers {
		if strings.ToLower(k) == "authorization" {
			callerHasAuthorization = true
		}
	}
	if len(headers) > 0 {
		lowered := map[string]bool{}
		for k := range headers {
			lowered[strings.ToLower(k)] = true
		}
		drop(func(k string) bool { return lowered[k] })
		for k, v := range headers {
			candidate[k] = v
		}
	}
	if key != "" && !callerHasAuthorization {
		drop(func(k string) bool { return k == "authorization" })
		candidate["Authorization"] = "Bearer " + key
	}
	var candidateOrNil map[string]string
	if len(candidate) > 0 {
		candidateOrNil = candidate
	}
	useNative := shouldUseOllamaNativeCatalogFn(ctx, nativeProvider, apiURL, candidateOrNil)
	resolved := headers
	if useNative {
		resolved = candidateOrNil
		if preserveNative {
			return CatalogRows{}, false
		}
		nativeURL := apiURL
		probe := func() (CatalogRows, bool) {
			models, ok := fetchOllamaLocalModelsFn(ctx, nativeURL, timeout, resolved)
			if !ok {
				return CatalogRows{}, false
			}
			return CatalogRows{IDs: nonNilStrings(models), Native: true}, true
		}
		var rows CatalogRows
		var ok bool
		if cache {
			rows, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: key, BaseURL: apiURL, Timeout: timeout, Headers: headers,
				APIMode: apiMode, FetchModels: probe, TTL: ollamaLocalModelsCacheTTL})
		} else {
			rows, ok = probe()
		}
		if ok {
			return rows, true
		}
		apiURL = normalizeOpenAIBaseURL(apiURL)
	}
	var generic CatalogRows
	var ok bool
	if cache {
		generic, ok = CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: key, BaseURL: apiURL, Timeout: timeout, Headers: resolved, APIMode: apiMode})
	} else {
		var ids []string
		ids, ok = FetchAPIModels(ctx, key, apiURL, timeout, apiMode, resolved)
		generic = CatalogRows{IDs: nonNilStrings(ids)}
	}
	if ok && (len(generic.IDs) > 0 || useNative) {
		generic.IDs = nonNilStrings(generic.IDs)
		return generic, true
	}
	return CatalogRows{}, false
}

func nonNilStrings(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// A provider credential is selectable now, a picker also accepting a pool whose entries all cool down
func credentialPoolIsUsable(ctx context.Context, provider string, rawPoolPresent, forPicker bool) bool {
	pool := pickerLoadPoolFn(ctx, provider)
	if pool != nil && pool.HasCredentials() {
		return forPicker || pool.HasAvailable("")
	}
	return rawPoolPresent
}

// Runs the picker listing once per process in the background so the disk catalog cache is warm
func PrewarmPickerCacheAsync(ctx context.Context) <-chan struct{} {
	if !pickerPrewarmDone.CompareAndSwap(false, true) {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pc, err := loadPickerContext(ctx)
		if err != nil {
			modelsLog.Debug("picker cache prewarm failed", "error", err)
			return
		}
		if _, err := ListAuthenticatedProviders(ctx, AuthenticatedProvidersOptions{CurrentProvider: pc.CurrentProvider,
			CurrentBaseURL: pc.CurrentBaseURL, CurrentModel: pc.CurrentModel, UserProviders: pc.UserProviders,
			CustomProviders: pc.CustomProviders, ExcludedProviders: pc.ExcludedProviders}); err != nil {
			modelsLog.Debug("picker cache prewarm failed", "error", err)
		}
	}()
	return done
}

// Fetches in parallel the provider catalogs the serial picker loop could not serve from disk
func prefetchProviderModelsParallel(ctx context.Context, providerSlugs []string) {
	now := modelsClock()
	cache := loadProviderModelsCacheFn(ctx)
	var stale []string
	for _, slug := range providerSlugs {
		key := normalizedCacheSlug(slug)
		if key != "" && diskServeTier(cache[key], credentialFingerprintFn(ctx, key), now, key == "ollama", providerModelsCacheTTL) == "" {
			stale = append(stale, key)
		}
	}
	if len(stale) == 0 {
		return
	}
	work := make(chan string)
	var wg sync.WaitGroup
	for range min(8, len(stale)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for slug := range work {
				cachedProviderModelIDsFn(ctx, slug, CachedModelsOptions{ForceRefresh: true})
			}
		}()
	}
	for _, slug := range stale {
		work <- slug
	}
	close(work)
	wg.Wait()
}

// Any of the variables reads non empty through readEnv
func anyEnv(envVars []string, readEnv func(string) (string, error)) (bool, error) {
	for _, name := range envVars {
		value, err := readEnv(name)
		if err != nil {
			return false, err
		}
		if value != "" {
			return true, nil
		}
	}
	return false, nil
}

func pickerScopedEnv(ctx context.Context) func(string) (string, error) {
	return func(name string) (string, error) { return scopedKeyEnv(ctx, name) }
}

func rawEnv(name string) (string, error) { return os.Getenv(name), nil }

// Any key lowercased is already emitted or excluded
func pickerSkip(seen, excluded map[string]bool, keys ...string) bool {
	for _, k := range keys {
		if seen[strings.ToLower(k)] || excluded[strings.ToLower(k)] {
			return true
		}
	}
	return false
}

// Auth registry row, nil for the openrouter and custom pseudo providers the hermes registry lacks
func authRegistryGet(ctx context.Context, key string) *Profile {
	if key == "" || key == "openrouter" || key == "custom" {
		return nil
	}
	row := registryLookup(ctx, key)
	if row == nil || row.Name == "openrouter" || row.Name == "custom" {
		return nil
	}
	return row
}

// Registry entry the auth flows read their portal, client id and scope from, a KeyError when unregistered
func providerConfig(ctx context.Context, key string) (*Profile, error) {
	if row := authRegistryGet(ctx, key); row != nil {
		return row, nil
	}
	return nil, fmt.Errorf("KeyError: %s", pyrepr.StrRepr(key))
}

// Runtime resolution recognizes the provider identity, a capability check rather than a credential check
func isRuntimeProviderRoutable(env PoolEnv, providerID string) (bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(providerID))
	if normalized == "" {
		return false, nil
	}
	switch normalized {
	case "auto", "openrouter", "custom", "moa":
		return true, nil
	}
	if strings.HasPrefix(normalized, "custom:") {
		return true, nil
	}
	if _, err := resolveProvider(env, normalized); err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

type builtinCandidate struct {
	hermesID, mdevID string
	pconfig          *Profile
	envVars          []string
}

// Visits each models.dev mapped api_key provider that is routable, unclaimed and not an aggregator alias
func iterBuiltinCandidates(ctx context.Context, data map[string]any, excluded, seen map[string]bool, visit func(builtinCandidate) error) error {
	env := modelsPoolEnv(ctx)
	mapping := ModelsDevProviderMap()
	aggregators := AggregatorProviders()
	for _, hermesID := range sortedStringKeys(mapping) {
		mdevID := mapping[hermesID]
		if target := providerDefAliases[hermesID]; target != "" && target != hermesID && containsString(aggregators, target) {
			continue
		}
		if prof := getProviderProfile(ctx, hermesID); prof != nil && prof.Name != hermesID {
			continue
		}
		if seen[strings.ToLower(hermesID)] || excluded[strings.ToLower(hermesID)] || excluded[strings.ToLower(mdevID)] {
			continue
		}
		pdata, ok := data[mdevID].(map[string]any)
		if !ok {
			continue
		}
		pconfig := authRegistryGet(ctx, hermesID)
		if pconfig != nil && pconfig.AuthType != "api_key" {
			continue
		}
		routable, err := isRuntimeProviderRoutable(env, hermesID)
		if err != nil {
			return err
		}
		if !routable {
			continue
		}
		var envVars []string
		if pconfig != nil && len(pconfig.KeyEnv) > 0 {
			envVars = append(envVars, pconfig.KeyEnv...)
		} else {
			raw, present := pdata["env"]
			if !present {
				raw = []any{}
			}
			list, isList := raw.([]any)
			if !isList {
				continue
			}
			for _, v := range list {
				envVars = append(envVars, pyStr(v))
			}
		}
		if err := visit(builtinCandidate{hermesID: hermesID, mdevID: mdevID, pconfig: pconfig, envVars: envVars}); err != nil {
			return err
		}
	}
	return nil
}

// auth.json carries a providers entry under any of the keys
func authStoreHasProvider(ctx context.Context, keys ...string) bool {
	data, err := modelsPoolEnv(ctx).Store.Load()
	if err != nil {
		modelsLog.Debug("Auth store check failed", "keys", keys, "error", err)
		return false
	}
	if len(data) == 0 {
		return false
	}
	providers, _ := data["providers"].(map[string]any)
	for _, k := range keys {
		if _, ok := providers[k]; ok {
			return true
		}
	}
	return false
}

// Section one pool check, consulted only when auth.json lists a raw pool entry
func rawPoolUsable(ctx context.Context, hermesID string, forPicker bool) bool {
	data, err := modelsPoolEnv(ctx).Store.Load()
	if err != nil || len(data) == 0 {
		return false
	}
	pool, _ := data["credential_pool"].(map[string]any)
	if pyTruthy(pool[hermesID]) {
		return credentialPoolIsUsable(ctx, hermesID, true, forPicker)
	}
	return false
}

// Vertex OAuth, extra env vars, registry key vars or the Azure Entra config mark an overlay configured
func overlayHasEnvCreds(ctx context.Context, pid, hermesSlug string, overlay IcebathOverlay, readEnv func(string) (string, error)) (bool, error) {
	hasCreds := false
	if overlay.AuthType == "vertex" {
		hasCreds = pickerVertexCredentialsFn(ctx)
	} else if len(overlay.ExtraEnvVars) > 0 {
		lit, err := anyEnv(overlay.ExtraEnvVars, readEnv)
		if err != nil {
			return false, err
		}
		hasCreds = lit
	}
	if !hasCreds && overlay.AuthType == "api_key" {
		for _, key := range []string{pid, hermesSlug} {
			if pcfg := authRegistryGet(ctx, key); pcfg != nil && len(pcfg.KeyEnv) > 0 {
				lit, err := anyEnv(pcfg.KeyEnv, readEnv)
				if err != nil {
					return false, err
				}
				if lit {
					return true, nil
				}
			}
		}
	}
	if !hasCreds && hermesSlug == "azure-foundry" {
		return azureEntraConfigured(ctx, readEnv)
	}
	return hasCreds, nil
}

// Azure Foundry under entra_id auth is configured once the provider, auth mode and an endpoint are set
func azureEntraConfigured(ctx context.Context, readEnv func(string) (string, error)) (bool, error) {
	modelCfg := modelConfigDictFn(ctx)
	if strings.ToLower(strings.TrimSpace(pyOrStr(modelCfg["provider"]))) != "azure-foundry" ||
		strings.ToLower(strings.TrimSpace(pyOrStr(modelCfg["auth_mode"]))) != "entra_id" {
		return false, nil
	}
	if strings.TrimSpace(pyOrStr(modelCfg["base_url"])) != "" {
		return true, nil
	}
	value, err := readEnv("AZURE_FOUNDRY_BASE_URL")
	return value != "", err
}

// Explicit AWS auth config is present in the environment, never probing instance metadata
func hasFastAWSSDKSignal() bool {
	set := func(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }
	if set("AWS_BEARER_TOKEN_BEDROCK") || (set("AWS_ACCESS_KEY_ID") && set("AWS_SECRET_ACCESS_KEY")) {
		return true
	}
	for _, name := range []string{"AWS_PROFILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_WEB_IDENTITY_TOKEN_FILE"} {
		if set(name) {
			return true
		}
	}
	return false
}

// AWS SDK credentials, the full chain consulted only for the current provider
func hasAWSSDKCredsForListing(slug, currentProvider string) bool {
	if hasFastAWSSDKSignal() {
		return true
	}
	if strings.ToLower(strings.TrimSpace(slug)) != strings.ToLower(strings.TrimSpace(currentProvider)) {
		return false
	}
	return pickerAWSCredentialsFn(true)
}

func isAWSSDK(pconfig *Profile) bool { return pconfig != nil && pconfig.AuthType == "aws_sdk" }

// Disk cached catalog, else the first curated list merged with models.dev for preferred providers
func liveOrCuratedIDs(ctx context.Context, slug string, curated map[string][]string, fallbackKeys []string, mergeModelsDev, nonBlocking bool) []string {
	ids := cachedProviderModelIDsFn(ctx, slug, CachedModelsOptions{NonBlocking: nonBlocking})
	if len(ids) == 0 {
		keys := fallbackKeys
		if len(keys) == 0 {
			keys = []string{slug}
		}
		ids = firstCurated(curated, keys)
		if mergeModelsDev && modelsDevPreferred[slug] {
			ids = mergeWithModelsDevFn(ctx, slug, ids)
		}
	}
	return nonNilStrings(WithoutGenerationModels(ids))
}

// First non empty curated list under the keys, else the last key's value
func firstCurated(curated map[string][]string, keys []string) []string {
	var ids []string
	for _, key := range keys {
		ids = curated[key]
		if len(ids) > 0 {
			break
		}
	}
	return cloneStrings(ids)
}

// Bedrock live discovery reflects the active region, the curated list answering otherwise
func awsLiveOrCuratedIDs(ctx context.Context, slug string, curated map[string][]string, fallbackKeys []string, nonBlocking bool) []string {
	return liveOrCuratedIDs(ctx, slug, curated, fallbackKeys, false, nonBlocking)
}

// Nous curated agentic list with Portal recommendations and sale models, narrowed by org policy, plan picks first
func nousPickerModelIDs(ctx context.Context, curated map[string][]string, forceFreshNousTier bool) []string {
	ids := cloneStrings(curated["nous"])
	pricing := pickerNousPricingFn(ctx)
	if pricing == nil {
		pricing = PricingTable{}
	}
	portal := ""
	if state, err := modelsPoolEnv(ctx).Store.GetProviderAuthState("nous"); err == nil {
		portal = pyOrStr(state["portal_base_url"])
	}
	if checkNousFreeTierFn(ctx, forceFreshNousTier, false) {
		ids, _ = unionPortalFreeFn(ctx, ids, pricing, portal, false)
	} else {
		ids, _ = unionPortalPaidFn(ctx, ids, pricing, portal, false)
		ids = UnionWithNousOnSaleModels(ids, pricing)
	}
	if allowed, ok := nousPolicyAllowedIDsFn(ctx, false); ok {
		ids = RestrictToNousPolicy(ids, allowed, true)
	}
	if usage := nousPickerUsageIn(ctx, false, true); usage != nil {
		ids = featuredFirst(ids, usage.FeaturedModelIDs)
	}
	return nonNilStrings(ids)
}

// Free tier rule for a Nous row, nil when guests are switched off
func freeTierNousRow(ctx context.Context, row map[string]any) map[string]any {
	env := modelsPoolEnv(ctx)
	if !hasGuest(env) {
		return row
	}
	if !guestAllowed(env) {
		return nil
	}
	if !profileFreeInferenceActive(env) {
		return row
	}
	out := make(map[string]any, len(row)+3)
	for k, v := range row {
		out[k] = v
	}
	out["name"] = FreeTierLabel
	out["models"] = []any{GuestModel}
	out["total_models"] = 1
	out["free_tier_row"] = true
	return out
}

// Applies max models, uncapped rows show everything while zero still means no models
func capModels(ids []string, maxModels *int, slug string) []string {
	if maxModels == nil || (*maxModels != 0 && uncappedPickerProviders[slug]) {
		return ids
	}
	n := *maxModels
	if n < 0 {
		n = max(0, len(ids)+n)
	}
	return ids[:min(n, len(ids))]
}

type pickerGroup struct {
	slug, name, apiURL, apiMode, credentialIdentity string
	models                                          []string
	hasExplicitModels, discoveryAllowed             bool
	apiKey                                          any
	headers                                         map[string]string
	rawNames                                        []string
	aliases                                         map[string]bool
}

// Folds an entry's active selection then declared models into its group, list shapes pinning the row
func absorbEntryModels(grp *pickerGroup, entry map[string]any, activeModel string) {
	grp.models = extendUnique(grp.models, []string{activeModel})
	modelsField := entry["models"]
	if modelsConfigIsAllowlist(modelsField, entryModelsDiscovered(entry)) {
		grp.hasExplicitModels = true
	}
	grp.models = extendUnique(grp.models, declaredModelIDs(modelsField))
	grp.models = splitChainEntries(grp.models)
}

// Comma separated fallback chains also list each member after the raw chain
func splitChainEntries(models []string) []string {
	for _, model := range append([]string(nil), models...) {
		if strings.Contains(model, ",") {
			parts := strings.Split(model, ",")
			for i := range parts {
				parts[i] = pyStripSpace(parts[i])
			}
			models = extendUnique(models, parts)
		}
	}
	return models
}

// Appends each non empty item not yet present, order preserved
func extendUnique(target, items []string) []string {
	for _, item := range items {
		if item != "" && !containsString(target, item) {
			target = append(target, item)
		}
	}
	return target
}

// Trimmed lowercase URL without trailing slashes
func normURL(url any) string {
	if !pyTruthy(url) {
		return ""
	}
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(pyStr(url)), "/"))
}

var defaultEntryURLKeys = []string{"base_url", "url", "api"}

// First non empty URL field of an entry
func entryBaseURL(entry map[string]any, keys []string) string {
	for _, key := range keys {
		if v := entry[key]; pyTruthy(v) {
			return pyStr(v)
		}
	}
	return ""
}

// Lowercased api_mode or transport, empty when neither is set
func entryAPIMode(entry map[string]any) string {
	v := entry["api_mode"]
	if !pyTruthy(v) {
		v = entry["transport"]
	}
	return strings.ToLower(strings.TrimSpace(pyOrStr(v)))
}

// Unminted credential, env fallback name and the stable grouping identity
func entryCredentials(ctx context.Context, entry map[string]any, keyEnvKeys ...string) (any, string, string) {
	keyEnv := ""
	for _, k := range keyEnvKeys {
		if pyTruthy(entry[k]) {
			keyEnv = strings.TrimSpace(pyStr(entry[k]))
			break
		}
	}
	label := "custom"
	if pyTruthy(entry["name"]) {
		label = pyStr(entry["name"])
	}
	if source := BuildCommandTokenSource(pyOrStr(entry["key_cmd"]), label, home.Dir(ctx), nil); source != nil {
		return source, keyEnv, source.CacheIdentity()
	}
	inline := strings.TrimSpace(pyOrStr(entry["api_key"]))
	identity := inline
	if identity == "" && keyEnv != "" {
		identity = "env:" + keyEnv
	}
	return inline, keyEnv, identity
}

// discover_models defaults true, the strings false, no and 0 meaning false
func discoverFlag(entry map[string]any) bool {
	discover, ok := entry["discover_models"]
	if !ok {
		return true
	}
	if s, isStr := discover.(string); isStr {
		switch strings.ToLower(s) {
		case "false", "no", "0":
			return false
		}
		return true
	}
	return pyTruthy(discover)
}

// Text before the per model separator the config writer uses
func displayPrefix(name string) string {
	for _, sep := range []string{"—", " - "} {
		if strings.Contains(name, sep) {
			return pyStripSpace(strings.SplitN(name, sep, 2)[0])
		}
	}
	return name
}

// Section three label cut at the first versioned token when two or more words remain
func groupDisplayName(displayName string) string {
	grp := displayPrefix(displayName)
	toks := strings.Fields(grp)
	for i, t := range toks {
		if strings.ContainsFunc(strings.Trim(t, ".,()"), unicode.IsDigit) {
			if i >= 2 {
				grp = pyStripSpace(strings.Join(toks[:i], " "))
			}
			break
		}
	}
	if grp == "" {
		return displayName
	}
	return grp
}

// Live probe or warm cache read of a custom endpoint, ok false when nothing usable was found
func discoverEndpointModels(ctx context.Context, apiKey any, apiURL, nativeProvider string, hasExplicitModels bool, headers map[string]string, apiMode string, probeLive, discoveryAllowed, fastCustomProbe bool) ([]string, bool, bool) {
	timeout := 5 * time.Second
	if fastCustomProbe {
		timeout = 1500 * time.Millisecond
	}
	if probeLive {
		live, ok := pickerFetchLiveModelsFn(ctx, apiKey, apiURL, nativeProvider, hasExplicitModels, headers, timeout, apiMode, true)
		isNative := ok && live.Native
		if isNative && hasExplicitModels {
			return nil, false, false
		}
		if ok && (len(live.IDs) > 0 || !hasExplicitModels || isNative) {
			return nonNilStrings(live.IDs), true, isNative && len(live.IDs) == 0
		}
	} else if discoveryAllowed {
		cached, ok := CachedFetchAPIModels(ctx, CachedFetchOptions{APIKey: apiKey, BaseURL: apiURL, CacheOnly: true, Timeout: timeout, Headers: headers, APIMode: apiMode})
		if hasExplicitModels && ok && cached.Native {
			return nil, false, false
		}
		if ok && (len(cached.IDs) > 0 || cached.Native) {
			return nonNilStrings(cached.IDs), true, cached.Native && len(cached.IDs) == 0
		}
	}
	return nil, false, false
}

// Quick credential scan of sections one, two and two b for the parallel prefetch, Nous left out
func collectAuthedProviderSlugs(ctx context.Context, data map[string]any, excluded []string) ([]string, error) {
	excludedSet := map[string]bool{}
	for _, p := range excluded {
		if p != "" {
			excludedSet[strings.ToLower(strings.TrimSpace(p))] = true
		}
	}
	var slugs []string
	seen := map[string]bool{}
	emit := func(slug string, keys ...string) {
		slugs = append(slugs, slug)
		for _, k := range keys {
			seen[strings.ToLower(k)] = true
		}
	}
	readEnv := pickerScopedEnv(ctx)
	err := iterBuiltinCandidates(ctx, data, excludedSet, seen, func(c builtinCandidate) error {
		lit, err := anyEnv(c.envVars, readEnv)
		if err != nil {
			return err
		}
		if lit || rawPoolUsable(ctx, c.hermesID, false) {
			emit(c.hermesID, c.hermesID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	mdevToHermes := invertStringMap(ModelsDevProviderMap())
	overlays := IcebathOverlays(ctx)
	for _, pid := range sortedStringKeys(overlays) {
		overlay := overlays[pid]
		hermesSlug := pid
		if mapped, ok := mdevToHermes[pid]; ok {
			hermesSlug = mapped
		}
		if pickerSkip(seen, excludedSet, pid, hermesSlug) || overlay.AuthType == "aws_sdk" {
			continue
		}
		has, err := overlayHasEnvCreds(ctx, pid, hermesSlug, overlay, readEnv)
		if err != nil {
			return nil, err
		}
		if has || authStoreHasProvider(ctx, pid, hermesSlug) || credentialPoolIsUsable(ctx, hermesSlug, false, false) {
			emit(hermesSlug, pid, hermesSlug)
		}
	}
	for _, cp := range pickerCanonicalFn() {
		if pickerSkip(seen, excludedSet, cp.Slug) {
			continue
		}
		hasCreds := false
		if cfg := authRegistryGet(ctx, cp.Slug); cfg != nil && len(cfg.KeyEnv) > 0 {
			lit, err := anyEnv(cfg.KeyEnv, readEnv)
			if err != nil {
				return nil, err
			}
			hasCreds = lit
		}
		if hasCreds || authStoreHasProvider(ctx, cp.Slug) || credentialPoolIsUsable(ctx, cp.Slug, false, false) {
			emit(cp.Slug, cp.Slug)
		}
	}
	out := make([]string, 0, len(slugs))
	for _, s := range slugs {
		if s != "nous" {
			out = append(out, s)
		}
	}
	return out, nil
}

// Insertion ordered inverse of a mapping, the later key winning a shared value as a dict comprehension does
func invertStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for _, k := range sortedStringKeys(m) {
		out[m[k]] = k
	}
	return out
}

// State threaded through the picker sections, from built ins to custom_providers entries
type pickerBuild struct {
	ctx                        context.Context
	currentProvider            string
	currentBaseURL             string
	currentModel               string
	maxModels                  *int
	forPicker                  bool
	forceFreshNousTier         bool
	probeCustomProviders       bool
	probeCurrentCustomProvider bool
	refresh                    bool
	excluded                   map[string]bool
	curated                    map[string][]string
	fastCustomProbe            *bool
	nonBlockingCatalogs        bool
	results                    []map[string]any
	seenSlugs                  map[string]bool
	builtinEndpoints           map[string]bool
	section3Pairs              map[[2]string]bool
}

func (b *pickerBuild) currentProviderNorm() string { return strings.ToLower(b.currentProvider) }

func (b *pickerBuild) currentBaseURLNorm() string {
	return strings.ToLower(strings.TrimRight(b.currentBaseURL, "/"))
}

// An unset override defers to the picker flag so existing callers keep their timeout
func (b *pickerBuild) resolvedFastCustomProbe() bool {
	if b.fastCustomProbe == nil {
		return b.forPicker
	}
	return *b.fastCustomProbe
}

func (b *pickerBuild) canProbeCustom(rowIsCurrent bool) bool {
	return b.probeCustomProviders || (b.probeCurrentCustomProvider && rowIsCurrent)
}

// Records a built in row's effective base URL, the live env override first
func (b *pickerBuild) recordBuiltinEndpoint(slug string) {
	pcfg := authRegistryGet(b.ctx, slug)
	if pcfg == nil {
		return
	}
	url := ""
	if pcfg.BaseURLEnv != "" {
		url = os.Getenv(pcfg.BaseURLEnv)
	}
	if url == "" {
		url = pcfg.BaseURL
	}
	if normed := normURL(url); normed != "" {
		b.builtinEndpoints[normed] = true
	}
}

func stringsToAny(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (b *pickerBuild) addBuiltinRow(slug, name string, isCurrent bool, modelIDs []string, source string, uncappedOK bool) {
	capSlug := ""
	if uncappedOK {
		capSlug = slug
	}
	row := map[string]any{"slug": slug, "name": name, "is_current": isCurrent, "is_user_defined": false,
		"models": stringsToAny(capModels(modelIDs, b.maxModels, capSlug)), "total_models": len(modelIDs), "source": source}
	if slug == "nous" {
		row = freeTierNousRow(b.ctx, row)
	}
	if row != nil {
		b.results = append(b.results, row)
	}
	b.seenSlugs[strings.ToLower(slug)] = true
	b.recordBuiltinEndpoint(slug)
}

// Appends a user defined endpoint row, shown replacing the listed models when given
func (b *pickerBuild) addEndpointRow(slug, name, apiURL string, models []string, isCurrent, nativeCatalogEmpty bool, source string, shown []string) {
	listed := models
	if shown != nil {
		listed = shown
	}
	b.results = append(b.results, map[string]any{"slug": slug, "name": name, "is_current": isCurrent, "is_user_defined": true,
		"models": stringsToAny(listed), "total_models": len(models), "source": source, "api_url": apiURL,
		"native_catalog_empty": nativeCatalogEmpty})
	b.seenSlugs[strings.ToLower(slug)] = true
}

// Remembers a name and base URL pair for section four dedup, false when either is blank
func (b *pickerBuild) recordSection3Pair(name, urlNorm string) bool {
	pair := [2]string{strings.ToLower(pyStripSpace(name)), urlNorm}
	if pair[0] == "" || pair[1] == "" {
		return false
	}
	b.section3Pairs[pair] = true
	return true
}

// Row is current by slug or alias, or for the bare custom provider by a matching base URL
func (b *pickerBuild) endpointIsCurrent(slug string, aliases map[string]bool, urlNorm string, urlMatchOK bool) bool {
	cur := b.currentProviderNorm()
	return strings.ToLower(strings.TrimSpace(slug)) == cur || aliases[cur] ||
		(cur == "custom" && b.currentBaseURLNorm() != "" && urlNorm == b.currentBaseURLNorm() && urlMatchOK)
}

// Probe policy of sections three and four, returning models, ok, native empty and whether it probed live
func (b *pickerBuild) discoverEndpoint(apiKey any, apiURL, nativeProvider string, hasExplicitModels bool, headers map[string]string, apiMode string, discoveryAllowed, isCurrent bool) ([]string, bool, bool, bool) {
	probeLive := discoveryAllowed && (pyTruthy(apiKey) || !hasExplicitModels) && b.canProbeCustom(isCurrent)
	models, ok, nativeEmpty := pickerDiscoverEndpointFn(b.ctx, apiKey, apiURL, nativeProvider, hasExplicitModels, headers, apiMode,
		probeLive, discoveryAllowed, b.resolvedFastCustomProbe())
	return models, ok, nativeEmpty, probeLive
}

func uniqueConcat(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return nonNilStrings(out)
}

// Section zero, the active or providers configured LM Studio row from the live catalog
func lapLMStudioRow(b *pickerBuild, userProviders map[string]any) {
	if b.excluded["lmstudio"] || b.seenSlugs["lmstudio"] {
		return
	}
	configured, isDict := userProviders["lmstudio"].(map[string]any)
	if isDict && entryBaseURL(configured, []string{"base_url", "api", "url"}) != "" {
		return
	}
	isCurrent := b.currentProviderNorm() == "lmstudio"
	if !isCurrent && !isDict {
		return
	}
	var configuredModels []string
	if isDict {
		configuredModels = declaredModelIDs(configured["models"])
	}
	b.addBuiltinRow("lmstudio", GetProviderLabel(b.ctx, "lmstudio"), isCurrent, uniqueConcat(configuredModels, b.curated["lmstudio"]), "hermes", true)
}

// Section one, models.dev mapped providers with api_key auth
func lapBuiltinRows(b *pickerBuild, data, userProviders map[string]any) error {
	readEnv := pickerScopedEnv(b.ctx)
	return iterBuiltinCandidates(b.ctx, data, b.excluded, b.seenSlugs, func(c builtinCandidate) error {
		lit, err := anyEnv(c.envVars, readEnv)
		if err != nil {
			return err
		}
		if !lit && !rawPoolUsable(b.ctx, c.hermesID, b.forPicker) {
			return nil
		}
		ids := liveOrCuratedIDs(b.ctx, c.hermesID, b.curated, nil, true, b.nonBlockingCatalogs)
		var configuredModels []string
		if configured, ok := userProviders[c.hermesID].(map[string]any); ok {
			configuredModels = declaredModelIDs(configured["models"])
		}
		ids = uniqueConcat(configuredModels, ids)
		displayName := c.mdevID
		if c.pconfig != nil && c.pconfig.DisplayName != "" {
			displayName = c.pconfig.DisplayName
		} else if info, ok := pickerProviderInfoFn(b.ctx, c.mdevID, true); ok {
			displayName = info.Name
		}
		b.addBuiltinRow(c.hermesID, displayName, b.currentProvider == c.hermesID || b.currentProvider == c.mdevID, ids, "built-in", true)
		return nil
	})
}

// Section two credential ladder, env or SDK, external process binary, auth store, pool, Claude credential files
func overlayHasCreds(b *pickerBuild, pid, hermesSlug string, overlay IcebathOverlay) (bool, error) {
	var hasCreds bool
	if overlay.AuthType == "aws_sdk" {
		hasCreds = hasAWSSDKCredsForListing(hermesSlug, b.currentProvider)
	} else {
		var err error
		if hasCreds, err = overlayHasEnvCreds(b.ctx, pid, hermesSlug, overlay, pickerScopedEnv(b.ctx)); err != nil {
			return false, err
		}
	}
	env := modelsPoolEnv(b.ctx)
	if !hasCreds && overlay.AuthType == "external_process" {
		status := GetAuthStatus(env, hermesSlug)
		hasCreds = pyTruthy(status["logged_in"]) || pyTruthy(status["configured"])
	}
	hasCreds = hasCreds || authStoreHasProvider(b.ctx, pid, hermesSlug)
	if !hasCreds && credentialPoolIsUsable(b.ctx, hermesSlug, false, b.forPicker) {
		hasCreds = true
	}
	if !hasCreds && hermesSlug == "anthropic" {
		own := ReadHermesOAuthCredentials(env)
		cc := ReadClaudeCodeCredentials(env)
		if pyTruthy(own["accessToken"]) || pyTruthy(cc["accessToken"]) {
			hasCreds = true
		}
	}
	return hasCreds, nil
}

// Section two, the provider overlays outside models.dev
func lapOverlayRows(b *pickerBuild, userProviders map[string]any) error {
	mdevToHermes := invertStringMap(ModelsDevProviderMap())
	overlays := IcebathOverlays(b.ctx)
	for _, pid := range sortedStringKeys(overlays) {
		overlay := overlays[pid]
		hermesSlug := pid
		if mapped, ok := mdevToHermes[pid]; ok {
			hermesSlug = mapped
		}
		if pickerSkip(b.seenSlugs, b.excluded, pid, hermesSlug) {
			continue
		}
		has, err := overlayHasCreds(b, pid, hermesSlug, overlay)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		var ids []string
		switch {
		case hermesSlug == "openai-codex":
			ids = liveOrCuratedIDs(b.ctx, hermesSlug, b.curated, nil, false, b.nonBlockingCatalogs)
		case overlay.AuthType == "aws_sdk":
			ids = awsLiveOrCuratedIDs(b.ctx, hermesSlug, b.curated, []string{hermesSlug, pid}, b.nonBlockingCatalogs)
		case hermesSlug == "nous":
			tierRow := freeTierNousRow(b.ctx, map[string]any{"name": GetProviderLabel(b.ctx, hermesSlug), "models": []any{}})
			ids = []string{}
			if tierRow != nil && len(tierRow["models"].([]any)) == 0 {
				ids = pickerNousModelIDsFn(b.ctx, b.curated, b.forceFreshNousTier)
			}
		default:
			ids = liveOrCuratedIDs(b.ctx, hermesSlug, b.curated, []string{hermesSlug, pid}, true, b.nonBlockingCatalogs)
		}
		configured := userProviders[hermesSlug]
		if !pyTruthy(configured) {
			configured = userProviders[pid]
		}
		if cfg, ok := configured.(map[string]any); ok {
			ids = uniqueConcat(declaredModelIDs(cfg["models"]), ids)
		}
		b.addBuiltinRow(hermesSlug, GetProviderLabel(b.ctx, hermesSlug), b.currentProvider == hermesSlug || b.currentProvider == pid, ids, "hermes", true)
		b.seenSlugs[strings.ToLower(pid)] = true
	}
	return nil
}

// Section two b, canonical providers the earlier sections missed
func lapCanonicalRows(b *pickerBuild) {
	env := modelsPoolEnv(b.ctx)
	for _, cp := range pickerCanonicalFn() {
		if pickerSkip(b.seenSlugs, b.excluded, cp.Slug) {
			continue
		}
		cpConfig := authRegistryGet(b.ctx, cp.Slug)
		hasCreds := false
		if cpConfig != nil && len(cpConfig.KeyEnv) > 0 {
			lit := map[string]bool{}
			for _, ev := range cpConfig.KeyEnv {
				if os.Getenv(ev) != "" {
					lit[ev] = true
				}
			}
			hasCreds = len(lit) > 0
			var sibVars map[string]bool
			if strings.HasSuffix(cp.Slug, "-cn") {
				if sib := authRegistryGet(b.ctx, strings.TrimSuffix(cp.Slug, "-cn")); sib != nil {
					sibVars = stringSet(sib.KeyEnv)
				}
			}
			own := stringSet(cpConfig.KeyEnv)
			if len(lit) > 0 && isSubset(lit, sibVars) && isSubset(sibVars, own) && len(sibVars) < len(own) && cp.Slug != b.currentProvider {
				continue
			}
		}
		hasCreds = hasCreds || authStoreHasProvider(b.ctx, cp.Slug) || credentialPoolIsUsable(b.ctx, cp.Slug, false, b.forPicker) ||
			(isAWSSDK(cpConfig) && hasAWSSDKCredsForListing(cp.Slug, b.currentProvider))
		if !hasCreds && cpConfig != nil && cpConfig.AuthType == "external_process" {
			hasCreds = pyTruthy(GetExternalProcessProviderStatus(env, cp.Slug)["configured"])
		}
		if !hasCreds {
			continue
		}
		var ids []string
		if isAWSSDK(cpConfig) {
			ids = awsLiveOrCuratedIDs(b.ctx, cp.Slug, b.curated, nil, b.nonBlockingCatalogs)
		} else {
			ids = liveOrCuratedIDs(b.ctx, cp.Slug, b.curated, nil, false, b.nonBlockingCatalogs)
		}
		b.addBuiltinRow(cp.Slug, cp.Label, cp.Slug == b.currentProvider, ids, "canonical", false)
	}
}

func stringSet(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}

func isSubset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// Grouping key of an endpoint, credential identity, api mode, sorted headers and an optional label
func endpointGroupKey(parts []string, headers map[string]string) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p)
		sb.WriteByte(0)
	}
	for _, k := range sortedStringKeys(headers) {
		fmt.Fprintf(&sb, "%s\x01%s\x02", k, headers[k])
	}
	return sb.String()
}

// Section three, providers entries grouped by endpoint, credential, wire protocol and headers
func lapUserProviderRows(b *pickerBuild, userProviders map[string]any) error {
	var order []string
	groups := map[string]*pickerGroup{}
	for _, epName := range sortedStringKeys(userProviders) {
		epCfg, ok := userProviders[epName].(map[string]any)
		if !ok || !IsProviderEnabled(epCfg) || b.seenSlugs[strings.ToLower(epName)] {
			continue
		}
		displayName := CoerceProviderID(epCfg["name"])
		if displayName == "" {
			displayName = epName
		}
		apiURL := entryBaseURL(epCfg, []string{"base_url", "api", "url"})
		inlineKey, keyEnv, credIdentity := entryCredentials(b.ctx, epCfg, "key_env", "api_key_env")
		headers := extraHeadersFromConfig(epCfg)
		key := endpointGroupKey([]string{normURL(apiURL), credIdentity, entryAPIMode(epCfg)}, headers)
		grp, exists := groups[key]
		if !exists {
			apiKey := inlineKey
			if !pyTruthy(apiKey) {
				scoped, err := scopedKeyEnv(b.ctx, keyEnv)
				if err != nil {
					return err
				}
				apiKey = scoped
			}
			grp = &pickerGroup{slug: epName, name: groupDisplayName(displayName), apiURL: apiURL, models: []string{},
				apiKey: apiKey, headers: headers, apiMode: pyOrStr(epCfg["api_mode"]),
				discoveryAllowed: apiURL != "" && discoverFlag(epCfg), aliases: map[string]bool{}}
			groups[key] = grp
			order = append(order, key)
		}
		active := epCfg["default_model"]
		if !pyTruthy(active) {
			active = epCfg["model"]
		}
		absorbEntryModels(grp, epCfg, pyOrStr(active))
		grp.rawNames = append(grp.rawNames, displayName)
		for alias := range CustomProviderAliases(displayName, epName) {
			grp.aliases[alias] = true
		}
	}
	for _, key := range order {
		grp := groups[key]
		models := cloneStrings(grp.models)
		if len(models) == 0 && baseURLHostMatches(strings.ToLower(strings.TrimSpace(grp.apiURL)), "api.openai.com") {
			models = cloneStrings(b.curated["openai"])
		}
		urlNorm := normURL(grp.apiURL)
		aliases := map[string]bool{}
		for alias := range grp.aliases {
			aliases[strings.ToLower(alias)] = true
		}
		isCurrent := b.endpointIsCurrent(grp.slug, aliases, urlNorm, true)
		native := "custom"
		if s := strings.ToLower(strings.TrimSpace(grp.slug)); s == "ollama" || s == "custom:ollama" {
			native = grp.slug
		}
		var headers map[string]string
		if len(grp.headers) > 0 {
			headers = grp.headers
		}
		discovered, ok, nativeEmpty, _ := b.discoverEndpoint(grp.apiKey, grp.apiURL, native, grp.hasExplicitModels, headers, grp.apiMode, grp.discoveryAllowed, isCurrent)
		if ok {
			models = discovered
		}
		b.addEndpointRow(grp.slug, grp.name, grp.apiURL, nonNilStrings(models), isCurrent, nativeEmpty, "user-config", nil)
		for alias := range aliases {
			b.seenSlugs[alias] = true
		}
		rawNames := grp.rawNames
		if len(rawNames) == 0 {
			rawNames = []string{grp.name}
		}
		for _, raw := range rawNames {
			if b.recordSection3Pair(raw, urlNorm) {
				b.seenSlugs[strings.ToLower(CustomProviderSlug(raw, ""))] = true
			}
		}
		b.recordSection3Pair(grp.name, urlNorm)
	}
	return nil
}

// Section three b, the bare model.provider custom endpoint with no named providers row
func lapBareCustomRow(b *pickerBuild, customProviders []any) {
	if b.seenSlugs["custom"] {
		return
	}
	var baseURL, defaultModel string
	if b.currentProviderNorm() == "custom" && b.currentBaseURL != "" {
		baseURL, defaultModel = b.currentBaseURL, b.currentModel
	} else {
		cfgModel, ok := LoadConfigSafe(home.Dir(b.ctx))["model"].(map[string]any)
		if !ok {
			return
		}
		baseURL = strings.TrimSpace(pyOrStr(cfgModel["base_url"]))
		if strings.ToLower(strings.TrimSpace(pyOrStr(cfgModel["provider"]))) != "custom" || baseURL == "" {
			return
		}
		dflt, present := cfgModel["default"]
		if !present {
			dflt = cfgModel["name"]
		}
		defaultModel = strings.TrimSpace(pyOrStr(dflt))
	}
	for _, item := range customProviders {
		if cp, ok := item.(map[string]any); ok && normURL(entryBaseURL(cp, defaultEntryURLKeys)) == normURL(baseURL) {
			return
		}
	}
	apiURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	isCurrent := b.currentProviderNorm() == "custom" && b.currentBaseURLNorm() == normURL(apiURL)
	models := []string{}
	if defaultModel != "" {
		models = []string{defaultModel}
	}
	discovered, ok, nativeEmpty := pickerDiscoverEndpointFn(b.ctx, "", apiURL, "custom", false, nil, "",
		b.refresh || (b.probeCurrentCustomProvider && isCurrent), true, b.resolvedFastCustomProbe())
	if ok {
		models = discovered
	}
	b.addEndpointRow("custom", "Custom endpoint", apiURL, models, isCurrent, nativeEmpty, "model-config", nonNilStrings(capModels(models, b.maxModels, "")))
}

// Section four, custom_providers entries grouped per endpoint, credential, wire, headers and display prefix
func lapCustomProviderRows(b *pickerBuild, customProviders []any) error {
	var order []string
	groups := map[string]*pickerGroup{}
	for _, item := range customProviders {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rawName := CoerceProviderID(entry["name"])
		apiURL := strings.TrimRight(strings.TrimSpace(entryBaseURL(entry, defaultEntryURLKeys)), "/")
		if rawName == "" || apiURL == "" {
			continue
		}
		inlineKey, keyEnv, credIdentity := entryCredentials(b.ctx, entry, "key_env")
		apiKey := inlineKey
		if !pyTruthy(apiKey) {
			scoped, err := scopedKeyEnv(b.ctx, keyEnv)
			if err != nil {
				return err
			}
			apiKey = scoped
		}
		apiMode := entryAPIMode(entry)
		headers := extraHeadersFromConfig(entry)
		prefix := displayPrefix(rawName)
		providerKey := strings.TrimSpace(pyOrStr(entry["provider_key"]))
		key := endpointGroupKey([]string{apiURL, credIdentity, apiMode, strings.ToLower(prefix)}, headers)
		displayName := prefix
		if displayName == "" {
			displayName = rawName
		}
		grp, exists := groups[key]
		if !exists {
			grp = &pickerGroup{slug: CustomProviderSlug(displayName, providerKey), name: displayName, apiURL: apiURL, apiKey: "",
				credentialIdentity: credIdentity, models: []string{}, discoveryAllowed: true, apiMode: apiMode, headers: headers,
				aliases: map[string]bool{}}
			groups[key] = grp
			order = append(order, key)
		}
		if !pyTruthy(grp.apiKey) {
			grp.apiKey = apiKey
		}
		grp.discoveryAllowed = grp.discoveryAllowed && discoverFlag(entry)
		for alias := range CustomProviderAliases(rawName, providerKey) {
			grp.aliases[alias] = true
		}
		absorbEntryModels(grp, entry, strings.TrimSpace(pyOrStr(entry["model"])))
	}
	section4 := map[string]bool{}
	currentURLGroups := 0
	for _, key := range order {
		if b.currentBaseURLNorm() != "" && normURL(groups[key].apiURL) == b.currentBaseURLNorm() {
			currentURLGroups++
		}
	}
	for _, key := range order {
		grp := groups[key]
		slug := grp.slug
		if b.seenSlugs[strings.ToLower(slug)] && !section4[strings.ToLower(slug)] {
			continue
		}
		if section4[strings.ToLower(slug)] {
			base, n := slug, 2
			for b.seenSlugs[strings.ToLower(fmt.Sprintf("%s-%d", base, n))] {
				n++
			}
			slug = fmt.Sprintf("%s-%d", base, n)
			grp.slug = slug
		}
		urlNorm := normURL(grp.apiURL)
		pair := [2]string{strings.ToLower(pyStripSpace(grp.name)), urlNorm}
		if pair[0] != "" && pair[1] != "" && b.section3Pairs[pair] {
			continue
		}
		if urlNorm != "" && b.builtinEndpoints[urlNorm] {
			continue
		}
		aliases := map[string]bool{}
		for alias := range grp.aliases {
			aliases[strings.ToLower(alias)] = true
		}
		isCurrent := b.endpointIsCurrent(slug, aliases, urlNorm, currentURLGroups == 1)
		native := "custom"
		if strings.ToLower(strings.TrimSpace(slug)) == "ollama" || strings.ToLower(strings.TrimSpace(grp.name)) == "ollama" {
			native = "ollama"
		}
		var headers map[string]string
		if len(grp.headers) > 0 {
			headers = grp.headers
		}
		discovered, ok, nativeEmpty, probed := b.discoverEndpoint(grp.apiKey, grp.apiURL, native, grp.hasExplicitModels, headers,
			grp.apiMode, grp.apiURL != "" && grp.discoveryAllowed, isCurrent)
		if ok {
			grp.models = discovered
			if probed {
				identity := grp.credentialIdentity
				pickerSaveDiscoveredFn(b.ctx, grp.apiURL, discovered, grp.apiMode, headers, &identity)
			}
		}
		b.addEndpointRow(slug, grp.name, grp.apiURL, nonNilStrings(grp.models), isCurrent, nativeEmpty, "user-config", nil)
		section4[strings.ToLower(slug)] = true
	}
	return nil
}

// Curated lists per provider plus the Nous manifest, Ollama Cloud and a live LM Studio probe
func buildCuratedLists(ctx context.Context, currentProvider, currentBaseURL, currentModel string, nonBlocking bool) (map[string][]string, error) {
	curated := map[string][]string{}
	for k, v := range CuratedCatalogRows() {
		curated[k] = cloneStrings(v)
	}
	openrouter := make([]string, 0, len(OpenRouterModels))
	for _, m := range OpenRouterModels {
		openrouter = append(openrouter, m.ID)
	}
	curated["openrouter"] = openrouter
	for _, p := range ListProviders(ctx) {
		if len(curated[p.Name]) > 0 {
			continue
		}
		rows := p.StaticModels
		if len(rows) == 0 {
			rows = p.FallbackModels
		}
		if len(rows) > 0 {
			curated[p.Name] = cloneStrings(rows)
		}
	}
	curated["nous"] = curatedNousModelIDsFn(ctx)
	if _, ok := curated["ollama-cloud"]; !ok {
		curated["ollama-cloud"] = nonNilStrings(pickerOllamaCloudFn(ctx, OllamaCloudFetch{CacheOnly: nonBlocking}))
	}
	isCurrentLMStudio := strings.ToLower(strings.TrimSpace(currentProvider)) == "lmstudio"
	if _, ok := curated["lmstudio"]; !ok && (os.Getenv("LM_API_KEY") != "" || os.Getenv("LM_BASE_URL") != "" || isCurrentLMStudio) {
		lmBase := os.Getenv("LM_BASE_URL")
		if lmBase == "" && isCurrentLMStudio && currentBaseURL != "" {
			lmBase = currentBaseURL
		}
		if lmBase == "" {
			lmBase = "http://127.0.0.1:1234/v1"
		}
		live, err := pickerLMStudioFn(ctx, os.Getenv("LM_API_KEY"), lmBase, 1500*time.Millisecond)
		if err != nil {
			var authErr *AuthError
			if !errors.As(err, &authErr) {
				return nil, err
			}
			live = nil
		}
		if len(live) == 0 && isCurrentLMStudio && currentModel != "" {
			live = []string{currentModel}
		}
		curated["lmstudio"] = nonNilStrings(live)
	}
	return curated, nil
}

// ListAuthenticatedProviders lists the providers holding credentials with their curated or discovered models
func ListAuthenticatedProviders(ctx context.Context, opts AuthenticatedProvidersOptions) ([]map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	nonBlocking := opts.NonBlockingCatalogs
	if opts.Refresh {
		nonBlocking = false
		ClearAllProviderModelsCache(ctx)
	}
	currentProvider := CoerceProviderID(opts.CurrentProvider)
	currentBaseURL := strings.TrimSpace(opts.CurrentBaseURL)
	currentModel := strings.TrimSpace(opts.CurrentModel)
	userProviders := StringifyProviderMap(opts.UserProviders)
	data := pickerFetchModelsDevFn(ctx, false)
	excluded := map[string]bool{}
	for _, p := range opts.ExcludedProviders {
		if p != "" {
			excluded[strings.ToLower(strings.TrimSpace(p))] = true
		}
	}
	curated, err := buildCuratedLists(ctx, currentProvider, currentBaseURL, currentModel, nonBlocking)
	if err != nil {
		return nil, err
	}
	b := &pickerBuild{ctx: ctx, currentProvider: currentProvider, currentBaseURL: currentBaseURL, currentModel: currentModel,
		maxModels: opts.MaxModels, forPicker: opts.ForPicker, forceFreshNousTier: opts.ForceFreshNousTier,
		probeCustomProviders: !opts.SkipCustomProviderProbe, probeCurrentCustomProvider: opts.ProbeCurrentCustomProvider,
		refresh: opts.Refresh, excluded: excluded, nonBlockingCatalogs: nonBlocking, fastCustomProbe: opts.FastCustomProbe,
		curated: curated, seenSlugs: map[string]bool{}, builtinEndpoints: map[string]bool{}, section3Pairs: map[[2]string]bool{}}
	if !opts.Refresh && !nonBlocking {
		slugs, err := collectAuthedProviderSlugs(ctx, data, opts.ExcludedProviders)
		if err != nil {
			return nil, err
		}
		if len(slugs) > 3 {
			pickerPrefetchFn(ctx, slugs)
		}
	}
	lapLMStudioRow(b, userProviders)
	if err := lapBuiltinRows(b, data, userProviders); err != nil {
		return nil, err
	}
	if err := lapOverlayRows(b, userProviders); err != nil {
		return nil, err
	}
	lapCanonicalRows(b)
	if len(userProviders) > 0 {
		if err := lapUserProviderRows(b, userProviders); err != nil {
			return nil, err
		}
	}
	lapBareCustomRow(b, opts.CustomProviders)
	if len(opts.CustomProviders) > 0 {
		if err := lapCustomProviderRows(b, opts.CustomProviders); err != nil {
			return nil, err
		}
	}
	return finalizePickerRows(b.results, userProviders, currentModel), nil
}

// Drops disabled providers, injects the current model into its row and sorts current first then by size
func finalizePickerRows(results []map[string]any, userProviders map[string]any, currentModel string) []map[string]any {
	disabled := map[string]bool{}
	for name, raw := range userProviders {
		if cfg, ok := raw.(map[string]any); ok && !IsProviderEnabled(cfg) {
			disabled[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	if len(disabled) > 0 {
		kept := results[:0:0]
		for _, r := range results {
			if !disabled[strings.ToLower(strings.TrimSpace(pyOrStr(r["provider_id"])))] && !disabled[strings.ToLower(strings.TrimSpace(pyOrStr(r["slug"])))] {
				kept = append(kept, r)
			}
		}
		results = kept
	}
	if currentModel != "" {
		for _, row := range results {
			if !pyTruthy(row["is_current"]) || pyTruthy(row["native_catalog_empty"]) {
				continue
			}
			models, _ := row["models"].([]any)
			if !anyListContains(models, currentModel) {
				if modelRequiresAccountDiscovery(pyOrStr(row["slug"]), currentModel) {
					break
				}
				row["models"] = append([]any{currentModel}, models...)
				total, ok := pyIsInt(row["total_models"])
				if !ok {
					total = len(models)
				}
				row["total_models"] = total + 1
			}
			break
		}
	}
	if results == nil {
		results = []map[string]any{}
	}
	sort.SliceStable(results, func(i, j int) bool {
		ci, cj := pyTruthy(results[i]["is_current"]), pyTruthy(results[j]["is_current"])
		if ci != cj {
			return ci
		}
		ti, _ := pyIsInt(results[i]["total_models"])
		tj, _ := pyIsInt(results[j]["total_models"])
		return ti > tj
	})
	return results
}

func anyListContains(list []any, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Puts the virtual MoA row first for interactive pickers, replacing any listed moa row
func prependMoAPickerProvider(ctx context.Context, providers []map[string]any, currentProvider string) []map[string]any {
	moa := moaProviderRow(ctx, currentProvider)
	if moa == nil {
		return providers
	}
	out := []map[string]any{moa}
	for _, p := range providers {
		if strings.ToLower(pyOrStr(p["slug"])) != "moa" {
			out = append(out, p)
		}
	}
	return out
}

// ListPickerProviders is the interactive picker listing, OpenRouter filtered live and empty rows dropped
func ListPickerProviders(ctx context.Context, opts PickerProvidersOptions) ([]map[string]any, error) {
	providers, err := pickerListAuthenticatedFn(ctx, AuthenticatedProvidersOptions{CurrentProvider: opts.CurrentProvider,
		CurrentBaseURL: opts.CurrentBaseURL, UserProviders: opts.UserProviders, CustomProviders: opts.CustomProviders,
		MaxModels: opts.MaxModels, CurrentModel: opts.CurrentModel, ForPicker: true, ExcludedProviders: opts.ExcludedProviders,
		NonBlockingCatalogs: opts.NonBlockingCatalogs, SkipCustomProviderProbe: opts.SkipCustomProviderProbe,
		ProbeCurrentCustomProvider: opts.ProbeCurrentCustomProvider})
	if err != nil {
		return nil, err
	}
	if opts.IncludeMoA {
		providers = prependMoAPickerProvider(ctx, providers, opts.CurrentProvider)
	}
	filtered := []map[string]any{}
	rows := []any{}
	for _, p := range providers {
		if strings.ToLower(pyOrStr(p["slug"])) == "openrouter" {
			var live []string
			for _, m := range pickerOpenRouterModelsFn(ctx, FetchModelsOptions{CacheOnly: opts.NonBlockingCatalogs}) {
				live = append(live, m.ID)
			}
			copied := make(map[string]any, len(p))
			for k, v := range p {
				copied[k] = v
			}
			copied["models"] = stringsToAny(capModels(nonNilStrings(live), opts.MaxModels, "openrouter"))
			copied["total_models"] = len(live)
			p = copied
		}
		isCustomEndpoint := pyTruthy(p["is_user_defined"]) && pyTruthy(p["api_url"])
		if pyTruthy(p["models"]) || isCustomEndpoint {
			filtered = append(filtered, p)
			rows = append(rows, p)
		}
	}
	DropUnofferableModelIDs(rows)
	attachWindowModelNotes(ctx, filtered)
	return filtered, nil
}

// Nous rows on a usage window plan carry Large or Free labels and, from a fresh snapshot, the limit line
func attachWindowModelNotes(ctx context.Context, providers []map[string]any) {
	var nous map[string]any
	for _, p := range providers {
		if strings.ToLower(pyOrStr(p["slug"])) == "nous" {
			nous = p
			break
		}
	}
	if nous == nil {
		return
	}
	fresh := nousPickerUsageIn(ctx, false, false)
	usage := fresh
	if usage == nil {
		usage = nousPickerUsageIn(ctx, false, true)
	}
	if usage == nil {
		return
	}
	pricing := PricingTableAsAny(GetPricingForProvider(ctx, "nous", "", false, true))
	models := []string{}
	if list, ok := nous["models"].([]any); ok {
		for _, m := range list {
			models = append(models, pyStr(m))
		}
	}
	labels := map[string]any{}
	for id, level := range usageTagLevels(models, pricing) {
		if label := UsageLevelLabel(level, ""); label != "" {
			labels[id] = label
		}
	}
	if len(labels) > 0 {
		nous["model_labels"] = labels
	}
	if fresh == nil {
		return
	}
	if line := usage.LimitLine(""); line != "" {
		nous["plan_line"] = line
	}
}
