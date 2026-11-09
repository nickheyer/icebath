package provider

import (
	"context"
	"time"
)

// Seams tests replace for the catalog, detection and cache paths, bound in init so no initialization cycle forms
var (
	cachedProviderModelIDsFn                 func(ctx context.Context, provider string, opts CachedModelsOptions) []string
	findOpenRouterSlugFn                     func(ctx context.Context, modelName string) string
	providerHasCredentialsFn                 func(ctx context.Context, provider string) bool
	detectStaticProviderForModelFn           func(ctx context.Context, modelName, currentProvider string) (string, string, bool)
	modelConfigDictFn                        func(ctx context.Context) map[string]any
	apiKeyCredentialsFn                      func(ctx context.Context, normalized string) (string, string)
	profileFetchModelsFn                     func(ctx context.Context, p Profile, apiKey, baseURL string, timeout time.Duration) ([]string, bool)
	profileLiveCatalogFn                     func(ctx context.Context, normalized string) (CatalogRows, bool)
	relayModelCatalogFn                      func(ctx context.Context, normalized, relay string) ([]string, bool)
	mergeWithModelsDevFn                     func(ctx context.Context, provider string, curated []string) []string
	listAgenticModelsFn                      func(ctx context.Context, provider string, allowNetwork bool) []string
	staticCatalogFn                          func(ctx context.Context, provider string) []string
	modelsProfileForFn                       func(name string) (Profile, bool)
	providerModelIDsFn                       func(ctx context.Context, provider string, forceRefresh bool) CatalogRows
	loadProviderModelsCacheFn                func(ctx context.Context) map[string]any
	saveProviderModelsCacheFn                func(ctx context.Context, data map[string]any)
	credentialFingerprintFn                  func(ctx context.Context, provider string) string
	customEndpointFingerprintFn              func(apiKey any, apiMode string, headers map[string]string) string
	fetchAPIModelsFn                         func(ctx context.Context, apiKey, baseURL string, timeout time.Duration, apiMode string, headers map[string]string) ([]string, bool)
	spawnSWRRefreshFn                        func(ctx context.Context, cacheKey string, refresh func() (map[string]any, bool))
	shouldUseOllamaNativeCatalogFn           func(ctx context.Context, provider, baseURL string, headers map[string]string) bool
	getOllamaNativeHeadersFn                 func(ctx context.Context, baseURL string, apiKey any) map[string]string
	fetchOllamaLocalModelsFn                 func(ctx context.Context, baseURL string, timeout time.Duration, headers map[string]string) ([]string, bool)
	azureFoundryRuntimeFn                    func(ctx context.Context, normalized string, modelCfg map[string]any) (*Runtime, error)
	azureProbeOpenAIModelsFn                 func(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) (bool, []string)
	providerAuthStatusFn                     func(ctx context.Context, provider string) (bool, bool)
	resolveCodexRuntimeCredentialsReadOnlyFn func(env PoolEnv) (map[string]any, error)
	windowPlanDefaultFn                      func(modelIDs []string, pricing map[string]any) string
	modelAliasIdentityFn                     func(nameLower string) (string, string, bool)
	fetchCodexModelsFromAPIFn                func(ctx context.Context, accessToken, baseURL string) []string
	nousPortalAccountIsFreeTierFn            func(ctx context.Context, forceFresh bool) (bool, error)
	checkNousFreeTierFn                      func(ctx context.Context, forceFresh, cachedOnly bool) bool
	curatedNousModelIDsFn                    func(ctx context.Context) []string
	nousPricingFn                            func(ctx context.Context) PricingTable
	nousPolicyAllowedIDsFn                   func(ctx context.Context, forceRefresh bool) (map[string]bool, bool)
	unionPortalFreeFn                        func(ctx context.Context, curated []string, pricing PricingTable, portalBaseURL string, forceRefresh bool) ([]string, PricingTable)
	unionPortalPaidFn                        func(ctx context.Context, curated []string, pricing PricingTable, portalBaseURL string, forceRefresh bool) ([]string, PricingTable)
	preferredSilentDefaultFn                 func(ctx context.Context, provider string) string
	fetchLiveCatalogIndexFn                  func(ctx context.Context, rawURL string, timeout time.Duration) ([]any, map[string]map[string]any, bool)
	curatedOpenRouterModelsFn                func(ctx context.Context) []CatalogModel
)

func init() { bindModelsSeams() }

// Points every seam back at the real implementation
func bindModelsSeams() {
	cachedProviderModelIDsFn = CachedProviderModelIDs
	findOpenRouterSlugFn = findOpenRouterSlug
	providerHasCredentialsFn = ProviderHasCredentials
	detectStaticProviderForModelFn = DetectStaticProviderForModel
	modelConfigDictFn = getModelConfigDict
	apiKeyCredentialsFn = apiKeyCredentials
	profileFetchModelsFn = profileFetchModels
	profileLiveCatalogFn = profileLiveCatalog
	relayModelCatalogFn = relayModelCatalog
	mergeWithModelsDevFn = mergeWithModelsDev
	listAgenticModelsFn = ListAgenticModels
	staticCatalogFn = StaticCatalog
	modelsProfileForFn = modelsProfileFor
	providerModelIDsFn = ProviderModelIDs
	loadProviderModelsCacheFn = loadProviderModelsCache
	saveProviderModelsCacheFn = saveProviderModelsCache
	credentialFingerprintFn = credentialFingerprint
	customEndpointFingerprintFn = customEndpointFingerprint
	fetchAPIModelsFn = FetchAPIModels
	spawnSWRRefreshFn = spawnSWRRefresh
	shouldUseOllamaNativeCatalogFn = shouldUseOllamaNativeCatalog
	getOllamaNativeHeadersFn = getOllamaNativeHeaders
	fetchOllamaLocalModelsFn = FetchOllamaLocalModels
	azureFoundryRuntimeFn = func(ctx context.Context, normalized string, modelCfg map[string]any) (*Runtime, error) {
		return modelsResolver(ctx).resolveAzureFoundryRuntime(normalized, modelCfg, "", nil, "", "")
	}
	azureProbeOpenAIModelsFn = func(ctx context.Context, baseURL, apiKey string, tokenProvider func(context.Context) (string, error)) (bool, []string) {
		return (&AzureDetector{}).probeOpenAIModels(ctx, baseURL, apiKey, tokenProvider)
	}
	providerAuthStatusFn = providerAuthStatus
	resolveCodexRuntimeCredentialsReadOnlyFn = resolveCodexRuntimeCredentialsReadOnly
	windowPlanDefaultFn = windowPlanDefault
	modelAliasIdentityFn = modelAliasIdentity
	fetchCodexModelsFromAPIFn = fetchCodexModelsFromAPI
	nousPortalAccountIsFreeTierFn = nousPortalAccountIsFreeTier
	checkNousFreeTierFn = CheckNousFreeTier
	curatedNousModelIDsFn = GetCuratedNousModelIDs
	nousPricingFn = func(ctx context.Context) PricingTable { return GetPricingForProvider(ctx, "nous", "", false, false) }
	nousPolicyAllowedIDsFn = NousPolicyAllowedIDs
	unionPortalFreeFn = UnionWithPortalFreeRecommendations
	unionPortalPaidFn = UnionWithPortalPaidRecommendations
	preferredSilentDefaultFn = GetPreferredSilentDefaultModel
	fetchLiveCatalogIndexFn = fetchLiveCatalogIndex
	curatedOpenRouterModelsFn = CuratedOpenRouterModels
}
