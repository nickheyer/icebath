package catalog

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	RowSourceBuiltIn     = "built-in"
	RowSourceOverlay     = "icebath"
	RowSourceCanonical   = "canonical"
	RowSourceUserConfig  = "user-config"
	RowSourceModelConfig = "model-config"
	RowSourceVirtual     = "virtual"

	fastCustomProbeTimeout = 1500 * time.Millisecond
	fullCustomProbeTimeout = 5 * time.Second
	lmStudioPickerTimeout  = 1500 * time.Millisecond
	lmStudioDefaultBaseURL = "http://127.0.0.1:1234/v1"
	prefetchWorkers        = 8
	prefetchMinimum        = 3
	providerLMStudio       = "lmstudio"
	providerMoA            = "moa"
	overlayAuthVertex      = "vertex"
	overlayAuthAWSSDK      = "aws_sdk"
	moaRowLabel            = "Mixture of Agents"
	moaRowWarning          = "Aggregator is the acting model billed for the run; references only advise once per user turn by default."
)

var uncappedPickerProviders = []string{"opencode-zen", "opencode-go", providers.ProviderPortal, providerOpenRouter}

var overlayOrder = []string{
	providerMoA, providerOpenRouter, providers.ProviderPortal, "openai-codex", "openai-api", "xai-oauth", "qwen-oauth", providerLMStudio,
	"anthropic", "zai", "kimi-for-coding", "stepfun", "minimax", "minimax-oauth", "minimax-cn", "deepseek", "alibaba",
	"alibaba-coding-plan", "vercel", "opencode", "opencode-go", "kilo", "huggingface", "novita", "xai", "nvidia", "xiaomi",
	"tencent-tokenhub", "tencent-tokenplan", "arcee", "commandcode", "gmi", "fireworks", "actual", "upstage",
	"nebius-token-factory", "ollama-cloud", providerAzureFoundry, "bedrock", "vertex",
}

var (
	discoverFalseWords = []string{"false", "no", "0"}
	enabledFalseWords  = []string{"false", "0", "no", "off"}
	awsProfileSignals  = []string{"AWS_PROFILE", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_WEB_IDENTITY_TOKEN_FILE"}
	moaLegacyKeys      = []string{"reference_models", "aggregator", "reference_temperature", "aggregator_temperature", "max_tokens", "reference_max_tokens", "fanout"}
	runtimeIdentities  = []string{"auto", providerOpenRouter, providerCustom, providerMoA}
	accountGatedSlugs  = []string{"openai", "openai-api", "openai-codex"}
	nativeOllamaSlugs  = []string{providerOllama, "custom:ollama"}
)

type ProviderRow struct {
	Slug               string
	Name               string
	Current            bool
	UserDefined        bool
	Models             []string
	TotalModels        int
	Source             string
	APIURL             string
	NativeCatalogEmpty bool
	FreeTierRow        bool
	Authenticated      bool
	AuthType           string
	Warning            string
	Featured           []string
	ModelLabels        map[string]string
	PlanLine           string
}

type PickerContext struct {
	CurrentProvider string
	CurrentModel    string
	CurrentBaseURL  string
	UserProviders   config.Map[config.ProviderEntry]
	CustomProviders []config.CustomEndpoint
	Excluded        []string
}

type ProvidersQuery struct {
	PickerContext
	ForceFreshPortalTier       bool
	MaxModels                  *int
	Refresh                    bool
	ProbeCustomProviders       bool
	ProbeCurrentCustomProvider bool
	ForPicker                  bool
	NonBlockingCatalogs        bool
	FastCustomProbe            *bool
}

type PickerQuery struct {
	PickerContext
	MaxModels                  *int
	IncludeMoA                 bool
	NonBlockingCatalogs        bool
	ProbeCustomProviders       bool
	ProbeCurrentCustomProvider bool
}

type pickerState struct {
	mu        sync.Mutex
	prewarmed bool
}

type liveModels struct {
	ids    []string
	native bool
	listed bool
}

type overlayRow struct {
	id      string
	overlay providers.Overlay
}

type builtinCandidate struct {
	id      string
	mdev    string
	row     providers.Row
	hasRow  bool
	envVars []string
}

type endpointGroup struct {
	slug     string
	name     string
	apiURL   string
	key      auth.Credential
	identity string
	models   []string
	explicit bool
	discover bool
	mode     string
	headers  map[string]string
	rawNames []string
	aliases  []string
}

type pickerBuild struct {
	c                *Catalog
	q                ProvidersQuery
	provider         string
	providerNorm     string
	baseURL          string
	baseURLNorm      string
	model            string
	nonBlocking      bool
	excluded         map[string]bool
	curated          map[string][]string
	results          []ProviderRow
	seen             map[string]bool
	builtinEndpoints map[string]bool
	pairs            map[[2]string]bool
}

func normURL(raw string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(raw), "/"))
}

func capModels(ids []string, limit *int, slug string) []string {
	if limit == nil || (*limit != 0 && slices.Contains(uncappedPickerProviders, slug)) {
		return ids
	}
	end := *limit
	if end < 0 {
		end = max(0, len(ids)+end)
	}
	return ids[:min(end, len(ids))]
}

func extendUnique(target []string, items ...string) []string {
	for _, item := range items {
		if item != "" && !slices.Contains(target, item) {
			target = append(target, item)
		}
	}
	return target
}

func splitChainEntries(models []string) []string {
	for _, model := range slices.Clone(models) {
		if !strings.Contains(model, ",") {
			continue
		}
		for _, part := range strings.Split(model, ",") {
			models = extendUnique(models, strings.TrimSpace(part))
		}
	}
	return models
}

func joinUnique(first, second []string) []string {
	return dedupe(append(slices.Clone(first), second...))
}

func lowerSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if key := strings.ToLower(strings.TrimSpace(value)); key != "" {
			out[key] = true
		}
	}
	return out
}

func skipKeys(seen, excluded map[string]bool, keys ...string) bool {
	for _, key := range keys {
		if lower := strings.ToLower(key); seen[lower] || excluded[lower] {
			return true
		}
	}
	return false
}

func displayPrefix(name string) string {
	for _, sep := range []string{"—", " - "} {
		if head, _, found := strings.Cut(name, sep); found {
			return strings.TrimSpace(head)
		}
	}
	return name
}

func groupDisplayName(displayName string) string {
	shown := displayPrefix(displayName)
	tokens := strings.Fields(shown)
	cut := slices.IndexFunc(tokens, func(token string) bool {
		return strings.IndexFunc(strings.Trim(token, ".,()"), unicode.IsDigit) >= 0
	})
	if cut >= 2 {
		shown = strings.TrimSpace(strings.Join(tokens[:cut], " "))
	}
	return firstNonEmpty(shown, displayName)
}

func scalarTruthy(s config.Scalar, falseWords []string, trim bool) bool {
	if s.IsZero() {
		return true
	}
	if value, ok := s.AsBool(); ok {
		return value
	}
	if number, ok := s.AsFloat(); ok {
		return number != 0
	}
	word := strings.ToLower(s.String())
	if trim {
		word = strings.TrimSpace(word)
	}
	return !slices.Contains(falseWords, word)
}

func providerEnabled(entry config.ProviderEntry) bool {
	return scalarTruthy(entry.Enabled, enabledFalseWords, true)
}

func declaredModelIDs(models config.ProviderModels) []string {
	ids := []string{}
	seen := map[string]bool{}
	for _, candidate := range models.IDs() {
		id := strings.TrimSpace(candidate)
		if key := strings.ToLower(id); id != "" && !seen[key] {
			seen[key] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func entryModelsDiscovered(entry config.ProviderEntry) bool {
	return entry.ModelsDiscovered || entry.Models.Discovered
}

func modelsAreAllowlist(entry config.ProviderEntry) bool {
	return !entryModelsDiscovered(entry) && entry.Models.IsList() && len(declaredModelIDs(entry.Models)) > 0
}

func entryAPIMode(entry config.ProviderEntry) string {
	return strings.ToLower(strings.TrimSpace(firstNonEmpty(entry.APIMode, entry.Transport)))
}

func entryHeaders(entry config.ProviderEntry) map[string]string {
	out := map[string]string{}
	for _, name := range entry.ExtraHeaders.Keys() {
		out[name], _ = entry.ExtraHeaders.Get(name)
	}
	return out
}

func headersOrNone(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func keyHasValue(key auth.Credential) bool {
	return key.Tokens != nil || !key.Key.Empty()
}

func (g *endpointGroup) absorb(entry config.ProviderEntry, activeModel string) {
	g.models = extendUnique(g.models, activeModel)
	if modelsAreAllowlist(entry) {
		g.explicit = true
	}
	g.models = splitChainEntries(extendUnique(g.models, declaredModelIDs(entry.Models)...))
}

func (c *Catalog) entryCredentials(ctx context.Context, entry config.ProviderEntry, keyEnvs ...string) (key auth.Credential, identity string) {
	keyEnv := strings.TrimSpace(firstNonEmpty(keyEnvs...))
	if source, ok := c.auth.CommandTokenSource(entry.KeyCmd, firstNonEmpty(entry.Name, providerCustom)); ok && source != nil {
		return auth.Credential{Kind: auth.CredToken, Tokens: source}, source.CacheIdentity()
	}
	inline := strings.TrimSpace(entry.APIKey)
	identity = inline
	if identity == "" && keyEnv != "" {
		identity = "env:" + keyEnv
	}
	if inline == "" && keyEnv != "" {
		inline = c.env(ctx, keyEnv)
	}
	if inline == "" {
		return auth.Credential{}, identity
	}
	return auth.Credential{Kind: auth.CredAPIKey, Key: auth.NewSecret(inline)}, identity
}

func (c *Catalog) anyEnv(ctx context.Context, names []string) bool {
	return slices.ContainsFunc(names, func(name string) bool { return name != "" && c.env(ctx, name) != "" })
}

func (c *Catalog) credentialPoolUsable(ctx context.Context, provider string, rawPoolPresent, forPicker bool) bool {
	pool, err := c.auth.Pool(ctx, provider)
	if err == nil && pool.HasCredentials() {
		if forPicker {
			return true
		}
		if available, err := pool.HasAvailable(ctx, ""); err == nil {
			return available
		}
	}
	return rawPoolPresent
}

func (c *Catalog) authStoreHasProvider(ctx context.Context, keys ...string) bool {
	for _, key := range keys {
		_, present, err := c.auth.State(ctx, key)
		if err != nil {
			slog.Debug("auth store check failed", "provider", key, "error", err)
			return false
		}
		if present {
			return true
		}
	}
	return false
}

func (c *Catalog) rawPoolUsable(ctx context.Context, provider string, forPicker bool) bool {
	pooled, err := c.auth.PoolProviders(ctx)
	if err != nil || !slices.Contains(pooled, provider) {
		return false
	}
	return c.credentialPoolUsable(ctx, provider, true, forPicker)
}

func (c *Catalog) runtimeRoutable(ctx context.Context, provider string) bool {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return false
	}
	if slices.Contains(runtimeIdentities, normalized) || strings.HasPrefix(normalized, customProviderPrefix) {
		return true
	}
	if target, ok := auth.ProviderAlias(normalized); ok {
		normalized = target
	} else if target, ok := c.tables(ctx).aliases[normalized]; ok {
		normalized = target
	}
	_, known := c.reg.RegistryRow(ctx, normalized)
	return known
}

func (c *Catalog) modelsDevPairs(ctx context.Context) []providerPair {
	return append(slices.Clone(providerToModelsDev), c.profileModelsDevIDs(ctx)...)
}

func (c *Catalog) modelsDevOwners(ctx context.Context) map[string]string {
	owners := map[string]string{}
	for _, pair := range c.modelsDevPairs(ctx) {
		owners[pair.mdev] = pair.provider
	}
	return owners
}

func (c *Catalog) orderedOverlays(ctx context.Context) []overlayRow {
	all := c.reg.Overlays(ctx)
	out := make([]overlayRow, 0, len(all))
	taken := map[string]bool{}
	add := func(id string) {
		if overlay, ok := all[id]; ok && !taken[id] {
			taken[id] = true
			out = append(out, overlayRow{id: id, overlay: overlay})
		}
	}
	for _, id := range overlayOrder {
		add(id)
	}
	for _, p := range c.reg.All(ctx) {
		if spec := p.Spec(); spec.Overlay != nil {
			add(firstNonEmpty(spec.ModelsDevID, spec.Name))
		}
	}
	var rest []string
	for id := range all {
		if !taken[id] {
			rest = append(rest, id)
		}
	}
	slices.Sort(rest)
	for _, id := range rest {
		add(id)
	}
	return out
}

func (c *Catalog) builtinCandidates(ctx context.Context, registry *mdRegistry, excluded, seen map[string]bool) iter.Seq[builtinCandidate] {
	return func(yield func(builtinCandidate) bool) {
		for _, pair := range c.modelsDevPairs(ctx) {
			if target, ok := providers.Alias(pair.provider); ok && target != pair.provider && c.isAggregatorProvider(ctx, target) {
				continue
			}
			if profile, ok := c.reg.Get(ctx, pair.provider); ok && profile.Spec().Name != pair.provider {
				continue
			}
			if lower := strings.ToLower(pair.provider); seen[lower] || excluded[lower] || excluded[strings.ToLower(pair.mdev)] {
				continue
			}
			if registry == nil {
				continue
			}
			listed := registry.providers[pair.mdev]
			if listed == nil || !listed.valid {
				continue
			}
			row, hasRow := c.reg.RegistryRow(ctx, pair.provider)
			if (hasRow && row.AuthType != authTypeAPIKey) || !c.runtimeRoutable(ctx, pair.provider) {
				continue
			}
			envVars := listed.env
			if hasRow && len(row.APIKeyEnvVars) > 0 {
				envVars = row.APIKeyEnvVars
			}
			if !yield(builtinCandidate{id: pair.provider, mdev: pair.mdev, row: row, hasRow: hasRow, envVars: envVars}) {
				return
			}
		}
	}
}

func (c *Catalog) azureEntraConfigured(ctx context.Context) bool {
	model := c.modelConfig(ctx)
	if strings.ToLower(strings.TrimSpace(model.Provider)) != providerAzureFoundry || strings.ToLower(strings.TrimSpace(model.AuthMode)) != "entra_id" {
		return false
	}
	return strings.TrimSpace(model.BaseURL) != "" || c.env(ctx, "AZURE_FOUNDRY_BASE_URL") != ""
}

func (c *Catalog) overlayHasEnvCreds(ctx context.Context, id, slug string, overlay providers.Overlay) bool {
	has := false
	switch {
	case overlay.AuthType == overlayAuthVertex:
		if owner, ok := c.reg.VertexOwner(); ok {
			vertex, err := owner.Vertex(ctx)
			if err != nil {
				slog.Debug("vertex credential check failed", "error", err)
			} else {
				has = vertex.HasCredentials(ctx)
			}
		}
	case len(overlay.ExtraEnv) > 0:
		has = c.anyEnv(ctx, overlay.ExtraEnv)
	}
	if !has && overlay.AuthType == authTypeAPIKey {
		for _, key := range []string{id, slug} {
			if row, ok := c.reg.RegistryRow(ctx, key); ok && c.anyEnv(ctx, row.APIKeyEnvVars) {
				return true
			}
		}
	}
	if !has && slug == providerAzureFoundry {
		has = c.azureEntraConfigured(ctx)
	}
	return has
}

func (c *Catalog) hasFastAWSSignal(ctx context.Context) bool {
	set := func(name string) bool { return c.env(ctx, name) != "" }
	return set("AWS_BEARER_TOKEN_BEDROCK") || (set("AWS_ACCESS_KEY_ID") && set("AWS_SECRET_ACCESS_KEY")) || slices.ContainsFunc(awsProfileSignals, set)
}

func (c *Catalog) hasAWSCredsForListing(ctx context.Context, slug, currentProvider string) bool {
	if c.hasFastAWSSignal(ctx) {
		return true
	}
	if strings.ToLower(strings.TrimSpace(slug)) != strings.ToLower(strings.TrimSpace(currentProvider)) {
		return false
	}
	owner, ok := c.reg.BedrockOwner()
	if !ok {
		return false
	}
	access, err := owner.Bedrock(ctx, true)
	return err == nil && access.HasCredentials(ctx)
}

func firstCurated(curated map[string][]string, keys []string) []string {
	var ids []string
	for _, key := range keys {
		if ids = curated[key]; len(ids) > 0 {
			break
		}
	}
	return ids
}

func (c *Catalog) liveOrCuratedIDs(ctx context.Context, slug string, curated map[string][]string, fallbackKeys []string, mergeModelsDev, nonBlocking bool) ([]string, error) {
	ids, err := c.cachedProviderModelIDs(ctx, slug, false, nonBlocking)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		if len(fallbackKeys) == 0 {
			fallbackKeys = []string{slug}
		}
		ids = firstCurated(curated, fallbackKeys)
		if mergeModelsDev && slices.Contains(modelsDevPreferred, slug) {
			ids = c.mergeWithModelsDev(ctx, slug, ids)
		}
	}
	return c.WithoutGenerationModels(ids), nil
}

func (c *Catalog) awsLiveOrCuratedIDs(ctx context.Context, slug string, curated map[string][]string, fallbackKeys []string, nonBlocking bool) []string {
	ids, err := c.liveOrCuratedIDs(ctx, slug, curated, fallbackKeys, false, nonBlocking)
	if err != nil {
		if len(fallbackKeys) == 0 {
			fallbackKeys = []string{slug}
		}
		ids = firstCurated(curated, fallbackKeys)
	}
	if ids == nil {
		return []string{}
	}
	return ids
}

func (c *Catalog) portalPickerModelIDs(ctx context.Context, curated map[string][]string, forceFreshTier bool) []string {
	ids := curated[providers.ProviderPortal]
	pricing := c.pricingForProvider(ctx, providers.ProviderPortal, "", false, true)
	if pricing == nil {
		pricing = newPriceTable()
	}
	portalURL := c.portalStateURL(ctx)
	if c.checkPortalFreeTier(ctx, forceFreshTier, false) {
		ids, _ = c.unionWithPortalRecommendations(ctx, "freeRecommendedModels", ids, pricing, portalURL, false, true)
	} else {
		ids, _ = c.unionWithPortalRecommendations(ctx, "paidRecommendedModels", ids, pricing, portalURL, false, false)
		ids = UnionWithPortalOnSaleModels(ids, pricing)
	}
	ids = RestrictToPortalPolicy(ids, c.portalPolicyAllowedIDs(ctx, false), true)
	ids, _ = c.featuredFirst(ctx, ids, PlanAnyAge)
	return ids
}

func (c *Catalog) freeTierPortalRow(ctx context.Context, row ProviderRow) (ProviderRow, bool) {
	if !c.portal.HasGuest(ctx) {
		return row, true
	}
	if !c.portal.GuestAllowed(ctx) {
		return ProviderRow{}, false
	}
	if !c.portal.FreeInferenceActive(ctx) {
		return row, true
	}
	row.Name, row.Models, row.TotalModels, row.FreeTierRow = portal.FreeTierLabel, []string{portal.GuestModel}, 1, true
	return row, true
}

func (c *Catalog) fetchPickerLiveModels(ctx context.Context, key auth.Credential, apiURL, nativeProvider string, preserveNative bool, headers map[string]string, timeout time.Duration, mode string, cache bool) (liveModels, error) {
	if key.Tokens != nil {
		var mu sync.Mutex
		var failure error
		ids, native, listed := c.cachedAPIModels(ctx, APIModelsQuery{
			Key: key, BaseURL: apiURL, Timeout: timeout, Mode: mode, Headers: headers,
			Fetch: func(ctx context.Context) ([]string, bool, bool) {
				minted := auth.Credential{Kind: auth.CredAPIKey, Key: key.ProbeKey(ctx)}
				live, err := c.fetchPickerLiveModels(ctx, minted, apiURL, nativeProvider, preserveNative, headers, timeout, mode, false)
				if err != nil {
					mu.Lock()
					failure = err
					mu.Unlock()
					return nil, false, false
				}
				return live.ids, live.native, live.listed
			},
		})
		mu.Lock()
		defer mu.Unlock()
		if failure != nil {
			return liveModels{}, failure
		}
		return liveModels{ids: ids, native: native, listed: listed}, nil
	}
	token := key.ProbeKey(ctx).Reveal()
	configured, err := c.local.OllamaNativeHeaders(ctx, apiURL, local.ProbeKey{Static: token})
	if err != nil {
		return liveModels{}, err
	}
	candidate := make(map[string]string, len(configured)+len(headers)+1)
	for name, value := range configured {
		candidate[name] = value
	}
	callerAuthorization := false
	for name := range headers {
		callerAuthorization = callerAuthorization || strings.EqualFold(name, "authorization")
		for existing := range candidate {
			if strings.EqualFold(existing, name) {
				delete(candidate, existing)
			}
		}
	}
	for name, value := range headers {
		candidate[name] = value
	}
	if token != "" && !callerAuthorization {
		dropAuthorization(candidate)
		candidate["Authorization"] = "Bearer " + token
	}
	probeHeaders := headersOrNone(candidate)
	useNative, err := c.local.ShouldUseOllamaNativeCatalog(ctx, nativeProvider, apiURL, probeHeaders)
	if err != nil {
		return liveModels{}, err
	}
	resolved := headers
	if useNative {
		resolved = probeHeaders
		if preserveNative {
			return liveModels{}, nil
		}
		probeNative := func(ctx context.Context) (liveModels, error) {
			models, reachable, err := c.local.ProbeOllamaLocalModels(ctx, apiURL, timeout, resolved)
			if err != nil || !reachable {
				return liveModels{}, err
			}
			return liveModels{ids: models, native: true, listed: true}, nil
		}
		var native liveModels
		if cache {
			var mu sync.Mutex
			var failure error
			ids, isNative, listed := c.cachedAPIModels(ctx, APIModelsQuery{
				Key: key, BaseURL: apiURL, Timeout: timeout, Mode: mode, Headers: headers, TTL: ollamaLocalModelsCacheTTL,
				Fetch: func(ctx context.Context) ([]string, bool, bool) {
					probed, err := probeNative(ctx)
					if err != nil {
						mu.Lock()
						failure = err
						mu.Unlock()
					}
					return probed.ids, probed.native, probed.listed
				},
			})
			mu.Lock()
			err = failure
			mu.Unlock()
			if err != nil {
				return liveModels{}, err
			}
			native = liveModels{ids: ids, native: isNative, listed: listed}
		} else if native, err = probeNative(ctx); err != nil {
			return liveModels{}, err
		}
		if native.listed {
			return native, nil
		}
		apiURL = local.NormalizeOpenAIBaseURL(apiURL)
	}
	var generic liveModels
	if cache {
		generic.ids, generic.native, generic.listed = c.cachedAPIModels(ctx, APIModelsQuery{
			Key: key, BaseURL: apiURL, Timeout: timeout, Mode: mode, Headers: resolved,
		})
	} else {
		generic.ids, generic.listed = c.fetchAPIModels(ctx, auth.NewSecret(token), apiURL, timeout, mode, resolved)
	}
	if len(generic.ids) > 0 || useNative {
		return generic, nil
	}
	return liveModels{}, nil
}

func (c *Catalog) discoverEndpointModels(ctx context.Context, key auth.Credential, apiURL, nativeProvider string, explicit bool, headers map[string]string, mode string, probeLive, discoveryAllowed, fast bool) (models []string, found, nativeEmpty bool) {
	timeout := fullCustomProbeTimeout
	if fast {
		timeout = fastCustomProbeTimeout
	}
	switch {
	case probeLive:
		live, err := c.fetchPickerLiveModels(ctx, key, apiURL, nativeProvider, explicit, headers, timeout, mode, true)
		if err != nil {
			slog.Debug("custom endpoint model discovery failed", "error", err)
			return nil, false, false
		}
		if live.listed && live.native && explicit {
			return nil, false, false
		}
		if live.listed && (len(live.ids) > 0 || !explicit || live.native) {
			return live.ids, true, live.native && len(live.ids) == 0
		}
	case discoveryAllowed:
		ids, native, listed := c.cachedAPIModels(ctx, APIModelsQuery{
			Key: key, BaseURL: apiURL, CacheOnly: true, Timeout: timeout, Mode: mode, Headers: headers,
		})
		if explicit && listed && native {
			return nil, false, false
		}
		if listed && (len(ids) > 0 || native) {
			return ids, true, native && len(ids) == 0
		}
	}
	return nil, false, false
}

func (b *pickerBuild) fastProbe() bool {
	if b.q.FastCustomProbe == nil {
		return b.q.ForPicker
	}
	return *b.q.FastCustomProbe
}

func (b *pickerBuild) canProbeCustom(rowIsCurrent bool) bool {
	return b.q.ProbeCustomProviders || (b.q.ProbeCurrentCustomProvider && rowIsCurrent)
}

func (b *pickerBuild) recordBuiltinEndpoint(ctx context.Context, slug string) {
	row, ok := b.c.reg.RegistryRow(ctx, slug)
	if !ok {
		return
	}
	override := ""
	if row.BaseURLEnvVar != "" {
		override = b.c.env(ctx, row.BaseURLEnvVar)
	}
	if normalized := normURL(firstNonEmpty(override, row.InferenceBaseURL)); normalized != "" {
		b.builtinEndpoints[normalized] = true
	}
}

func (b *pickerBuild) addBuiltinRow(ctx context.Context, slug, name string, current bool, ids []string, source string, uncappedOK bool) {
	capSlug := ""
	if uncappedOK {
		capSlug = slug
	}
	row := ProviderRow{
		Slug: slug, Name: name, Current: current, Models: slices.Clone(capModels(ids, b.q.MaxModels, capSlug)),
		TotalModels: len(ids), Source: source,
	}
	keep := true
	if slug == providers.ProviderPortal {
		row, keep = b.c.freeTierPortalRow(ctx, row)
	}
	if keep {
		b.results = append(b.results, row)
	}
	b.seen[strings.ToLower(slug)] = true
	b.recordBuiltinEndpoint(ctx, slug)
}

func (b *pickerBuild) addEndpointRow(slug, name, apiURL string, models, shown []string, current, nativeEmpty bool, source string) {
	b.results = append(b.results, ProviderRow{
		Slug: slug, Name: name, Current: current, UserDefined: true, Models: slices.Clone(shown), TotalModels: len(models),
		Source: source, APIURL: apiURL, NativeCatalogEmpty: nativeEmpty,
	})
	b.seen[strings.ToLower(slug)] = true
}

func (b *pickerBuild) recordPair(name, urlNorm string) bool {
	pair := [2]string{strings.ToLower(strings.TrimSpace(name)), urlNorm}
	if pair[0] == "" || pair[1] == "" {
		return false
	}
	b.pairs[pair] = true
	return true
}

func (b *pickerBuild) endpointIsCurrent(slug string, aliases []string, urlNorm string, urlMatchOK bool) bool {
	return strings.ToLower(strings.TrimSpace(slug)) == b.providerNorm || slices.Contains(aliases, b.providerNorm) ||
		(b.providerNorm == providerCustom && b.baseURLNorm != "" && urlNorm == b.baseURLNorm && urlMatchOK)
}

func (b *pickerBuild) discoverEndpoint(ctx context.Context, key auth.Credential, apiURL, nativeProvider string, explicit bool, headers map[string]string, mode string, discoveryAllowed, current bool) (models []string, found, nativeEmpty, probed bool) {
	probed = discoveryAllowed && (keyHasValue(key) || !explicit) && b.canProbeCustom(current)
	models, found, nativeEmpty = b.c.discoverEndpointModels(ctx, key, apiURL, nativeProvider, explicit, headers, mode, probed, discoveryAllowed, b.fastProbe())
	return models, found, nativeEmpty, probed
}

func (b *pickerBuild) userEntry(names ...string) (config.ProviderEntry, bool) {
	for _, name := range names {
		if entry, ok := b.q.UserProviders.Get(name); ok && entry.Valid() {
			return entry, true
		}
	}
	return config.ProviderEntry{}, false
}

func (b *pickerBuild) lapLMStudioRow(ctx context.Context) {
	if b.excluded[providerLMStudio] || b.seen[providerLMStudio] {
		return
	}
	configured, isConfigured := b.userEntry(providerLMStudio)
	if isConfigured && firstNonEmpty(configured.BaseURL, configured.API, configured.URL) != "" {
		return
	}
	current := b.providerNorm == providerLMStudio
	if !current && !isConfigured {
		return
	}
	var declared []string
	if isConfigured {
		declared = declaredModelIDs(configured.Models)
	}
	b.addBuiltinRow(ctx, providerLMStudio, b.c.Label(ctx, providerLMStudio), current, joinUnique(declared, b.curated[providerLMStudio]), RowSourceOverlay, true)
}

func (b *pickerBuild) lapBuiltinRows(ctx context.Context, registry *mdRegistry) error {
	c := b.c
	for candidate := range c.builtinCandidates(ctx, registry, b.excluded, b.seen) {
		if !c.anyEnv(ctx, candidate.envVars) && !c.rawPoolUsable(ctx, candidate.id, b.q.ForPicker) {
			continue
		}
		ids, err := c.liveOrCuratedIDs(ctx, candidate.id, b.curated, nil, true, b.nonBlocking)
		if err != nil {
			return err
		}
		if configured, ok := b.userEntry(candidate.id); ok {
			ids = joinUnique(declaredModelIDs(configured.Models), ids)
		} else {
			ids = dedupe(ids)
		}
		name := candidate.mdev
		if candidate.hasRow && candidate.row.Name != "" {
			name = candidate.row.Name
		} else if info, ok := c.providerInfo(ctx, candidate.mdev, true, nil); ok {
			name = info.Name
		}
		b.addBuiltinRow(ctx, candidate.id, name, b.provider == candidate.id || b.provider == candidate.mdev, ids, RowSourceBuiltIn, true)
	}
	return nil
}

func (b *pickerBuild) overlayHasCreds(ctx context.Context, id, slug string, overlay providers.Overlay) bool {
	c := b.c
	has := false
	if overlay.AuthType == overlayAuthAWSSDK {
		has = c.hasAWSCredsForListing(ctx, slug, b.provider)
	} else {
		has = c.overlayHasEnvCreds(ctx, id, slug, overlay)
	}
	if !has && overlay.AuthType == auth.AuthTypeExternalProcess {
		status, err := c.auth.Status(ctx, slug)
		if err != nil {
			slog.Debug("external process check failed", "provider", id, "error", err)
		} else {
			has = status.LoggedIn || status.Configured
		}
	}
	has = has || c.authStoreHasProvider(ctx, id, slug)
	if !has {
		has = c.credentialPoolUsable(ctx, slug, false, b.q.ForPicker)
	}
	if !has && slug == "anthropic" {
		has = c.auth.HasAnthropicOAuthFileCredentials(ctx)
	}
	return has
}

func (b *pickerBuild) lapOverlayRows(ctx context.Context) error {
	c := b.c
	owners := c.modelsDevOwners(ctx)
	for _, entry := range c.orderedOverlays(ctx) {
		id, overlay := entry.id, entry.overlay
		slug, mapped := owners[id]
		if !mapped {
			slug = id
		}
		if skipKeys(b.seen, b.excluded, id, slug) || !b.overlayHasCreds(ctx, id, slug, overlay) {
			continue
		}
		var ids []string
		var err error
		switch {
		case slug == "openai-codex":
			ids, err = c.liveOrCuratedIDs(ctx, slug, b.curated, nil, false, b.nonBlocking)
		case overlay.AuthType == overlayAuthAWSSDK:
			ids = c.awsLiveOrCuratedIDs(ctx, slug, b.curated, []string{slug, id}, b.nonBlocking)
		case slug == providers.ProviderPortal:
			tier, kept := c.freeTierPortalRow(ctx, ProviderRow{Name: c.Label(ctx, slug)})
			ids = []string{}
			if kept && len(tier.Models) == 0 {
				ids = c.portalPickerModelIDs(ctx, b.curated, b.q.ForceFreshPortalTier)
			}
		default:
			ids, err = c.liveOrCuratedIDs(ctx, slug, b.curated, []string{slug, id}, true, b.nonBlocking)
		}
		if err != nil {
			return err
		}
		if configured, ok := b.userEntry(slug, id); ok {
			ids = joinUnique(declaredModelIDs(configured.Models), ids)
		}
		b.addBuiltinRow(ctx, slug, c.Label(ctx, slug), b.provider == slug || b.provider == id, ids, RowSourceOverlay, true)
		b.seen[strings.ToLower(id)] = true
	}
	return nil
}

func (b *pickerBuild) lapCanonicalRows(ctx context.Context) error {
	c := b.c
	for _, entry := range c.tables(ctx).canonical {
		slug := entry.Slug
		if skipKeys(b.seen, b.excluded, slug) {
			continue
		}
		row, hasRow := c.reg.RegistryRow(ctx, slug)
		has := false
		if hasRow && len(row.APIKeyEnvVars) > 0 {
			var lit []string
			for _, name := range row.APIKeyEnvVars {
				if c.env(ctx, name) != "" {
					lit = append(lit, name)
				}
			}
			has = len(lit) > 0
			var siblingVars []string
			if strings.HasSuffix(slug, "-cn") {
				if sibling, ok := c.reg.RegistryRow(ctx, strings.TrimSuffix(slug, "-cn")); ok {
					siblingVars = sibling.APIKeyEnvVars
				}
			}
			within := func(inner, outer []string) bool {
				return !slices.ContainsFunc(inner, func(name string) bool { return !slices.Contains(outer, name) })
			}
			sharedOnly := within(lit, siblingVars) && within(siblingVars, row.APIKeyEnvVars) && !within(row.APIKeyEnvVars, siblingVars)
			if has && sharedOnly && slug != b.provider {
				continue
			}
		}
		awsSDK := hasRow && row.AuthType == overlayAuthAWSSDK
		has = has || c.authStoreHasProvider(ctx, slug) || c.credentialPoolUsable(ctx, slug, false, b.q.ForPicker) ||
			(awsSDK && c.hasAWSCredsForListing(ctx, slug, b.provider))
		if !has && hasRow && row.AuthType == auth.AuthTypeExternalProcess {
			status, err := c.auth.ExternalProcessStatus(ctx, slug)
			if err != nil {
				slog.Debug("external process check failed", "provider", slug, "error", err)
			} else {
				has = status.Configured
			}
		}
		if !has {
			continue
		}
		var ids []string
		if awsSDK {
			ids = c.awsLiveOrCuratedIDs(ctx, slug, b.curated, nil, b.nonBlocking)
		} else {
			var err error
			if ids, err = c.liveOrCuratedIDs(ctx, slug, b.curated, nil, false, b.nonBlocking); err != nil {
				return err
			}
		}
		b.addBuiltinRow(ctx, slug, entry.Label, slug == b.provider, ids, RowSourceCanonical, false)
	}
	return nil
}

func (b *pickerBuild) lapUserProviderRows(ctx context.Context) {
	c := b.c
	var order []string
	groups := map[string]*endpointGroup{}
	for _, name := range b.q.UserProviders.Keys() {
		entry, ok := b.q.UserProviders.Get(name)
		if !ok || !entry.Valid() || !providerEnabled(entry) || b.seen[strings.ToLower(name)] {
			continue
		}
		displayName := firstNonEmpty(strings.TrimSpace(entry.Name), name)
		apiURL := firstNonEmpty(entry.BaseURL, entry.API, entry.URL)
		key, identity := c.entryCredentials(ctx, entry, entry.KeyEnv, entry.APIKeyEnv)
		headers := entryHeaders(entry)
		groupKey := strings.Join([]string{normURL(apiURL), identity, entryAPIMode(entry), spacedJSONObject(headers)}, "\x00")
		group, exists := groups[groupKey]
		if !exists {
			group = &endpointGroup{
				slug: name, name: groupDisplayName(displayName), apiURL: apiURL, key: key, headers: headers, mode: entry.APIMode,
				discover: apiURL != "" && scalarTruthy(entry.DiscoverModels, discoverFalseWords, false),
			}
			groups[groupKey] = group
			order = append(order, groupKey)
		}
		group.absorb(entry, firstNonEmpty(entry.DefaultModel, entry.Model))
		group.rawNames = append(group.rawNames, displayName)
		group.aliases = extendUnique(group.aliases, CustomProviderAliases(displayName, name)...)
	}
	for _, groupKey := range order {
		group := groups[groupKey]
		models := slices.Clone(group.models)
		if len(models) == 0 && providers.HostMatches(strings.ToLower(strings.TrimSpace(group.apiURL)), "api.openai.com") {
			models = slices.Clone(b.curated["openai"])
		}
		urlNorm := normURL(group.apiURL)
		current := b.endpointIsCurrent(group.slug, group.aliases, urlNorm, true)
		nativeProvider := providerCustom
		if slices.Contains(nativeOllamaSlugs, strings.ToLower(strings.TrimSpace(group.slug))) {
			nativeProvider = group.slug
		}
		discovered, found, nativeEmpty, _ := b.discoverEndpoint(ctx, group.key, group.apiURL, nativeProvider, group.explicit,
			headersOrNone(group.headers), group.mode, group.discover, current)
		if found {
			models = discovered
		}
		b.addEndpointRow(group.slug, group.name, group.apiURL, models, models, current, nativeEmpty, RowSourceUserConfig)
		for _, alias := range group.aliases {
			b.seen[alias] = true
		}
		for _, rawName := range group.rawNames {
			if b.recordPair(rawName, urlNorm) {
				b.seen[strings.ToLower(CustomProviderSlug(rawName, ""))] = true
			}
		}
		b.recordPair(group.name, urlNorm)
	}
}

func (b *pickerBuild) lapBareCustomRow(ctx context.Context) {
	c := b.c
	if b.seen[providerCustom] {
		return
	}
	baseURL, defaultModel := "", ""
	if b.providerNorm == providerCustom && b.baseURL != "" {
		baseURL, defaultModel = b.baseURL, b.model
	} else {
		cfg := c.config(ctx)
		if cfg == nil || cfg.Model.Bare {
			return
		}
		baseURL = strings.TrimSpace(cfg.Model.BaseURL)
		if strings.ToLower(strings.TrimSpace(cfg.Model.Provider)) != providerCustom || baseURL == "" {
			return
		}
		defaultModel = strings.TrimSpace(firstNonEmpty(cfg.Model.Default.ID, cfg.Model.Name.ID))
	}
	for _, entry := range b.q.CustomProviders {
		if normURL(firstNonEmpty(entry.Text("base_url"), entry.Text("url"), entry.Text("api"))) == normURL(baseURL) {
			return
		}
	}
	apiURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	current := b.providerNorm == providerCustom && b.baseURLNorm == normURL(apiURL)
	models := []string{}
	if defaultModel != "" {
		models = append(models, defaultModel)
	}
	probeLive := b.q.Refresh || (b.q.ProbeCurrentCustomProvider && current)
	discovered, found, nativeEmpty := c.discoverEndpointModels(ctx, auth.Credential{}, apiURL, providerCustom, false, nil, "", probeLive, true, b.fastProbe())
	if found {
		models = discovered
	}
	b.addEndpointRow(providerCustom, customProviderLabel, apiURL, models, capModels(models, b.q.MaxModels, ""), current, nativeEmpty, RowSourceModelConfig)
}

func decodeEndpoint(entry config.CustomEndpoint) config.ProviderEntry {
	var typed config.ProviderEntry
	if err := typed.UnmarshalYAML(entry.Node()); err != nil {
		slog.Debug("custom endpoint entry carries a field of the wrong shape", "error", err)
	}
	return typed
}

func (b *pickerBuild) lapCustomProviderRows(ctx context.Context) {
	c := b.c
	var order []string
	groups := map[string]*endpointGroup{}
	for _, endpoint := range b.q.CustomProviders {
		rawName := strings.TrimSpace(endpoint.Text("name"))
		apiURL := strings.TrimRight(strings.TrimSpace(firstNonEmpty(endpoint.Text("base_url"), endpoint.Text("url"), endpoint.Text("api"))), "/")
		if rawName == "" || apiURL == "" {
			continue
		}
		entry := decodeEndpoint(endpoint)
		key, identity := c.entryCredentials(ctx, entry, entry.KeyEnv)
		mode := entryAPIMode(entry)
		headers := entryHeaders(entry)
		prefix := displayPrefix(rawName)
		providerKey := strings.TrimSpace(endpoint.Text("provider_key"))
		groupKey := strings.Join([]string{apiURL, identity, mode, spacedJSONObject(headers), strings.ToLower(prefix)}, "\x00")
		displayName := firstNonEmpty(prefix, rawName)
		group, exists := groups[groupKey]
		if !exists {
			group = &endpointGroup{
				slug: CustomProviderSlug(displayName, providerKey), name: displayName, apiURL: apiURL, identity: identity,
				discover: true, mode: mode, headers: headers,
			}
			groups[groupKey] = group
			order = append(order, groupKey)
		}
		if !keyHasValue(group.key) {
			group.key = key
		}
		group.discover = group.discover && scalarTruthy(entry.DiscoverModels, discoverFalseWords, false)
		group.aliases = extendUnique(group.aliases, CustomProviderAliases(rawName, providerKey)...)
		group.absorb(entry, strings.TrimSpace(entry.Model))
	}
	emitted := map[string]bool{}
	currentURLGroups := 0
	for _, groupKey := range order {
		if b.baseURLNorm != "" && normURL(groups[groupKey].apiURL) == b.baseURLNorm {
			currentURLGroups++
		}
	}
	for _, groupKey := range order {
		group := groups[groupKey]
		slug := group.slug
		lower := strings.ToLower(slug)
		if b.seen[lower] && !emitted[lower] {
			continue
		}
		if emitted[lower] {
			suffix := 2
			for b.seen[strings.ToLower(slug+"-"+strconv.Itoa(suffix))] {
				suffix++
			}
			slug += "-" + strconv.Itoa(suffix)
			group.slug = slug
		}
		urlNorm := normURL(group.apiURL)
		pair := [2]string{strings.ToLower(strings.TrimSpace(group.name)), urlNorm}
		if pair[0] != "" && pair[1] != "" && b.pairs[pair] {
			continue
		}
		if urlNorm != "" && b.builtinEndpoints[urlNorm] {
			continue
		}
		current := b.endpointIsCurrent(slug, group.aliases, urlNorm, currentURLGroups == 1)
		nativeProvider := providerCustom
		if strings.ToLower(strings.TrimSpace(slug)) == providerOllama || strings.ToLower(strings.TrimSpace(group.name)) == providerOllama {
			nativeProvider = providerOllama
		}
		discovered, found, nativeEmpty, probed := b.discoverEndpoint(ctx, group.key, group.apiURL, nativeProvider, group.explicit,
			headersOrNone(group.headers), group.mode, group.apiURL != "" && group.discover, current)
		if found {
			group.models = discovered
			if probed {
				c.saveDiscoveredModels(ctx, group.apiURL, discovered, group.mode, headersOrNone(group.headers), group.identity)
			}
		}
		b.addEndpointRow(slug, group.name, group.apiURL, group.models, group.models, current, nativeEmpty, RowSourceUserConfig)
		emitted[strings.ToLower(slug)] = true
	}
}

func modelsNodeOf(models config.ProviderModels) *yaml.Node {
	encoded, err := models.MarshalYAML()
	if err != nil {
		return nil
	}
	node, _ := encoded.(*yaml.Node)
	return node
}

func discoveredCatalogStale(entry config.ProviderEntry, ids []string) bool {
	legacy := entry.Models.Discovered
	discovered := entry.ModelsDiscovered || legacy
	if entry.Models.IsList() {
		node := modelsNodeOf(entry.Models)
		if node == nil || node.Kind != yaml.SequenceNode {
			return false
		}
		same := len(node.Content) == len(ids)
		for i, item := range node.Content {
			if item.Kind == yaml.MappingNode {
				return false
			}
			same = same && item.Kind == yaml.ScalarNode && item.ShortTag() == "!!str" && item.Value == ids[i]
		}
		return !same
	}
	if entry.Models.IsZero() {
		return true
	}
	return discovered && (legacy || entry.Models.ExplicitAllowlist || !slices.Equal(entry.Models.IDs(), ids))
}

func (c *Catalog) saveDiscoveredModels(ctx context.Context, apiURL string, ids []string, mode string, headers map[string]string, identity string) {
	if apiURL == "" || len(ids) == 0 {
		return
	}
	locked, unlock := c.cfg.Lock(ctx)
	defer unlock()
	cfg, err := c.cfg.Load(locked)
	if err != nil {
		slog.Debug("discovered models were not saved to config", "error", err)
		return
	}
	target := normURL(apiURL)
	changed := false
	entries := cfg.Extra.CustomProviders.Entries
	for i := range entries {
		entry := entries[i]
		if !entry.Valid() {
			continue
		}
		entryURL := strings.TrimSpace(firstNonEmpty(entry.BaseURL, entry.URL))
		if strings.ToLower(strings.TrimRight(entryURL, "/")) != target || entryAPIMode(entry) != mode {
			continue
		}
		if headers != nil && spacedJSONObject(entryHeaders(entry)) != spacedJSONObject(headers) {
			continue
		}
		if _, entryIdentity := c.entryCredentials(locked, entry, entry.KeyEnv, entry.APIKeyEnv); entryIdentity != identity {
			continue
		}
		if !discoveredCatalogStale(entry, ids) {
			continue
		}
		fresh := config.ProviderModels{}
		for _, id := range ids {
			fresh.Set(id, config.ProviderModel{})
		}
		entries[i].Models, entries[i].ModelsDiscovered = fresh, true
		changed = true
	}
	if !changed {
		return
	}
	if err := c.cfg.Save(locked, cfg); err != nil {
		slog.Debug("discovered models were not saved to config", "error", err)
	}
}

func (c *Catalog) buildCuratedLists(ctx context.Context, currentProvider, currentBaseURL, currentModel string, nonBlocking bool) (map[string][]string, error) {
	curated := map[string][]string{}
	for _, row := range staticRows {
		ids, _ := c.builtinStaticRow(ctx, row.provider)
		curated[row.provider] = ids
	}
	curated[providerOpenRouter] = choiceIDs(openRouterSnapshot)
	for _, p := range c.reg.All(ctx) {
		spec := p.Spec()
		if len(curated[spec.Name]) > 0 {
			continue
		}
		rows := spec.StaticModels
		if len(rows) == 0 {
			rows = spec.FallbackModels
		}
		if len(rows) > 0 {
			curated[spec.Name] = slices.Clone(rows)
		}
	}
	curated[providers.ProviderPortal] = c.curatedPortalModelIDs(ctx)
	if _, ok := curated["ollama-cloud"]; !ok {
		curated["ollama-cloud"] = c.local.FetchOllamaCloudModels(ctx, local.OllamaCloudRequest{
			CacheOnly: nonBlocking,
			List:      c.ListAPIModels,
			ModelsDev: func(ctx context.Context) ([]string, error) {
				return c.ListAgenticModels(ctx, "ollama-cloud", true), nil
			},
		})
	}
	currentLMStudio := strings.ToLower(strings.TrimSpace(currentProvider)) == providerLMStudio
	lmKey, lmBase := c.env(ctx, "LM_API_KEY"), c.env(ctx, "LM_BASE_URL")
	if _, ok := curated[providerLMStudio]; !ok && (lmKey != "" || lmBase != "" || currentLMStudio) {
		base := lmBase
		if base == "" && currentLMStudio {
			base = currentBaseURL
		}
		if base == "" {
			base = lmStudioDefaultBaseURL
		}
		live, err := c.local.FetchLMStudioModels(ctx, local.ProbeKey{Static: lmKey}, base, lmStudioPickerTimeout)
		if err != nil {
			var authErr *auth.Error
			if !errors.As(err, &authErr) {
				return nil, err
			}
			live = nil
		}
		if len(live) == 0 && currentLMStudio && currentModel != "" {
			live = []string{currentModel}
		}
		if live == nil {
			live = []string{}
		}
		curated[providerLMStudio] = live
	}
	return curated, nil
}

func (c *Catalog) collectAuthedProviderSlugs(ctx context.Context, registry *mdRegistry, excluded map[string]bool) []string {
	var slugs []string
	seen := map[string]bool{}
	emit := func(slug string, keys ...string) {
		slugs = append(slugs, slug)
		for _, key := range keys {
			seen[strings.ToLower(key)] = true
		}
	}
	for candidate := range c.builtinCandidates(ctx, registry, excluded, seen) {
		if c.anyEnv(ctx, candidate.envVars) || c.rawPoolUsable(ctx, candidate.id, false) {
			emit(candidate.id, candidate.id)
		}
	}
	owners := c.modelsDevOwners(ctx)
	for _, entry := range c.orderedOverlays(ctx) {
		slug, mapped := owners[entry.id]
		if !mapped {
			slug = entry.id
		}
		if skipKeys(seen, excluded, entry.id, slug) || entry.overlay.AuthType == overlayAuthAWSSDK {
			continue
		}
		if c.overlayHasEnvCreds(ctx, entry.id, slug, entry.overlay) || c.authStoreHasProvider(ctx, entry.id, slug) || c.credentialPoolUsable(ctx, slug, false, false) {
			emit(slug, entry.id, slug)
		}
	}
	for _, entry := range c.tables(ctx).canonical {
		if skipKeys(seen, excluded, entry.Slug) {
			continue
		}
		row, hasRow := c.reg.RegistryRow(ctx, entry.Slug)
		has := hasRow && c.anyEnv(ctx, row.APIKeyEnvVars)
		if has || c.authStoreHasProvider(ctx, entry.Slug) || c.credentialPoolUsable(ctx, entry.Slug, false, false) {
			emit(entry.Slug, entry.Slug)
		}
	}
	return slices.DeleteFunc(slugs, func(slug string) bool { return slug == providers.ProviderPortal })
}

func (c *Catalog) prefetchProviderModels(ctx context.Context, slugs []string) {
	now := unixSeconds(time.Now())
	cache := c.loadProviderModelsCache()
	var stale []string
	for _, slug := range slugs {
		key := c.normalizedCacheSlug(ctx, slug)
		if key == "" {
			continue
		}
		if diskServeTier(parseCacheRow(cache[key]), c.credentialFingerprint(ctx, key), now, key == providerOllama, providerModelsCacheTTL) == "" {
			stale = append(stale, key)
		}
	}
	if len(stale) == 0 {
		return
	}
	slots := make(chan struct{}, min(prefetchWorkers, len(stale)))
	var wg sync.WaitGroup
	for _, slug := range stale {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if _, err := c.cachedProviderModelIDs(ctx, slug, true, false); err != nil {
				slog.Debug("provider models cache prefetch failed", "provider", slug, "error", err)
			}
		}()
	}
	wg.Wait()
}

func (c *Catalog) modelRequiresAccountDiscovery(ctx context.Context, provider, model string) bool {
	return slices.Contains(accountGatedSlugs, c.normalizedCacheSlug(ctx, provider)) && wire.IsAstraModel(model)
}

func (c *Catalog) finalizePickerRows(ctx context.Context, rows []ProviderRow, user config.Map[config.ProviderEntry], currentModel string) []ProviderRow {
	disabled := map[string]bool{}
	for _, name := range user.Keys() {
		if entry, ok := user.Get(name); ok && entry.Valid() && !providerEnabled(entry) {
			disabled[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	if len(disabled) > 0 {
		rows = slices.DeleteFunc(rows, func(row ProviderRow) bool { return disabled[strings.ToLower(strings.TrimSpace(row.Slug))] })
	}
	if currentModel != "" {
		for i := range rows {
			row := &rows[i]
			if !row.Current || row.NativeCatalogEmpty {
				continue
			}
			if !slices.Contains(row.Models, currentModel) {
				if c.modelRequiresAccountDiscovery(ctx, row.Slug, currentModel) {
					break
				}
				row.Models = append([]string{currentModel}, row.Models...)
				row.TotalModels++
			}
			break
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Current != rows[j].Current {
			return rows[i].Current
		}
		return rows[i].TotalModels > rows[j].TotalModels
	})
	return rows
}

func (c *Catalog) authenticatedProviders(ctx context.Context, q ProvidersQuery) ([]ProviderRow, error) {
	nonBlocking := q.NonBlockingCatalogs
	if q.Refresh {
		nonBlocking = false
		c.ClearModelsCache(ctx, "")
	}
	b := &pickerBuild{
		c: c, q: q, nonBlocking: nonBlocking,
		provider: strings.TrimSpace(q.CurrentProvider), baseURL: strings.TrimSpace(q.CurrentBaseURL), model: strings.TrimSpace(q.CurrentModel),
		excluded: lowerSet(q.Excluded), results: []ProviderRow{}, seen: map[string]bool{},
		builtinEndpoints: map[string]bool{}, pairs: map[[2]string]bool{},
	}
	b.providerNorm = strings.ToLower(b.provider)
	b.baseURLNorm = strings.ToLower(strings.TrimRight(b.baseURL, "/"))
	registry := c.modelsDevRegistry(ctx, true)
	curated, err := c.buildCuratedLists(ctx, b.provider, b.baseURL, b.model, nonBlocking)
	if err != nil {
		return nil, err
	}
	b.curated = curated
	if !q.Refresh && !nonBlocking {
		if slugs := c.collectAuthedProviderSlugs(ctx, registry, b.excluded); len(slugs) > prefetchMinimum {
			c.prefetchProviderModels(ctx, slugs)
		}
	}
	b.lapLMStudioRow(ctx)
	if err := b.lapBuiltinRows(ctx, registry); err != nil {
		return nil, err
	}
	if err := b.lapOverlayRows(ctx); err != nil {
		return nil, err
	}
	if err := b.lapCanonicalRows(ctx); err != nil {
		return nil, err
	}
	if q.UserProviders.Len() > 0 {
		b.lapUserProviderRows(ctx)
	}
	b.lapBareCustomRow(ctx)
	if len(q.CustomProviders) > 0 {
		b.lapCustomProviderRows(ctx)
	}
	return c.finalizePickerRows(ctx, b.results, q.UserProviders, b.model), nil
}

func (c *Catalog) AuthenticatedProviders(ctx context.Context, q ProvidersQuery) ([]ProviderRow, error) {
	return c.authenticatedProviders(c.scoped(ctx), q)
}

func nodeTruthy(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	switch n.Kind {
	case yaml.MappingNode, yaml.SequenceNode:
		return len(n.Content) > 0
	case yaml.AliasNode:
		return nodeTruthy(n.Alias)
	}
	if n.ShortTag() == "!!null" {
		return false
	}
	var scalar config.Scalar
	if err := scalar.UnmarshalYAML(n); err != nil {
		return n.Value != ""
	}
	if value, ok := scalar.AsBool(); ok {
		return value
	}
	if number, ok := scalar.AsFloat(); ok {
		return number != 0
	}
	return scalar.String() != ""
}

func (c *Catalog) rawConfigEnablesMoA(ctx context.Context) bool {
	doc, err := c.cfg.RawDocumentReadonly(ctx)
	if err != nil || doc == nil {
		return false
	}
	moa, ok := doc.Get(providerMoA)
	if !ok || moa == nil || moa.Kind != yaml.MappingNode {
		return false
	}
	enabledOrUnset := func(section *yaml.Node) bool {
		flag := mappingChild(section, "enabled")
		return flag == nil || nodeTruthy(flag)
	}
	if presets := mappingChild(moa, "presets"); presets != nil && presets.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(presets.Content); i += 2 {
			if strings.TrimSpace(presets.Content[i].Value) == "" {
				continue
			}
			if preset := presets.Content[i+1]; preset.Kind != yaml.MappingNode || enabledOrUnset(preset) {
				return true
			}
		}
		return false
	}
	legacy := slices.ContainsFunc(moaLegacyKeys, func(key string) bool { return mappingChild(moa, key) != nil })
	return legacy && enabledOrUnset(moa)
}

func (c *Catalog) moaProviderRow(ctx context.Context, currentProvider string) (ProviderRow, bool) {
	if !c.rawConfigEnablesMoA(ctx) {
		return ProviderRow{}, false
	}
	cfg := c.config(ctx)
	if cfg == nil {
		return ProviderRow{}, false
	}
	presets := cfg.MoA.Presets.Keys()
	if len(presets) == 0 {
		return ProviderRow{}, false
	}
	return ProviderRow{
		Slug: providerMoA, Name: moaRowLabel, Current: strings.ToLower(currentProvider) == providerMoA, Models: slices.Clone(presets),
		TotalModels: len(presets), Source: RowSourceVirtual, Authenticated: true, AuthType: RowSourceVirtual, Warning: moaRowWarning,
	}, true
}

func (c *Catalog) MoAProviderRow(ctx context.Context, currentProvider string) (ProviderRow, bool) {
	return c.moaProviderRow(c.scoped(ctx), currentProvider)
}

func (c *Catalog) attachWindowModelNotes(ctx context.Context, rows []ProviderRow) {
	at := slices.IndexFunc(rows, func(row ProviderRow) bool { return strings.ToLower(row.Slug) == providers.ProviderPortal })
	if at < 0 {
		return
	}
	labels, planLine := c.plan.ModelNotes(ctx, slices.Clone(rows[at].Models))
	if len(labels) > 0 {
		rows[at].ModelLabels = labels
	}
	if planLine != "" {
		rows[at].PlanLine = planLine
	}
}

func (c *Catalog) PickerProviders(ctx context.Context, q PickerQuery) ([]ProviderRow, error) {
	ctx = c.scoped(ctx)
	rows, err := c.authenticatedProviders(ctx, ProvidersQuery{
		PickerContext: q.PickerContext, MaxModels: q.MaxModels, ForPicker: true, NonBlockingCatalogs: q.NonBlockingCatalogs,
		ProbeCustomProviders: q.ProbeCustomProviders, ProbeCurrentCustomProvider: q.ProbeCurrentCustomProvider,
	})
	if err != nil {
		return nil, err
	}
	if q.IncludeMoA {
		if moa, ok := c.moaProviderRow(ctx, q.CurrentProvider); ok {
			rows = append([]ProviderRow{moa}, slices.DeleteFunc(rows, func(row ProviderRow) bool { return strings.ToLower(row.Slug) == providerMoA })...)
		}
	}
	filtered := make([]ProviderRow, 0, len(rows))
	for _, row := range rows {
		if strings.ToLower(row.Slug) == providerOpenRouter {
			live := choiceIDs(c.fetchOpenRouterModels(ctx, openRouterListTimeout, false, q.NonBlockingCatalogs))
			row.Models, row.TotalModels = slices.Clone(capModels(live, q.MaxModels, providerOpenRouter)), len(live)
		}
		if len(row.Models) > 0 || (row.UserDefined && row.APIURL != "") {
			filtered = append(filtered, row)
		}
	}
	c.dropUnofferableModelIDs(ctx, filtered)
	c.attachWindowModelNotes(ctx, filtered)
	return filtered, nil
}

func (c *Catalog) excludedProviders(ctx context.Context) []string {
	doc, err := c.cfg.UserEffective(ctx, false)
	if err != nil || doc == nil {
		return nil
	}
	node, ok := doc.Get("model_catalog.excluded_providers")
	if !ok || node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var out []string
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode && item.ShortTag() != "!!null" && item.Value != "" {
			out = append(out, item.Value)
		}
	}
	return out
}

func (c *Catalog) loadPickerContext(ctx context.Context) (PickerContext, error) {
	cfg, err := c.cfg.LoadReadonly(ctx)
	if err != nil {
		return PickerContext{}, err
	}
	custom, err := c.cfg.CustomEndpoints(ctx)
	if err != nil {
		return PickerContext{}, err
	}
	out := PickerContext{
		CurrentModel: firstNonEmpty(cfg.Model.Default.ID, cfg.Model.Name.ID), UserProviders: cfg.Providers, CustomProviders: custom,
		Excluded: c.excludedProviders(ctx),
	}
	if !cfg.Model.Bare {
		out.CurrentProvider, out.CurrentBaseURL = strings.TrimSpace(cfg.Model.Provider), cfg.Model.BaseURL
	}
	return out, nil
}

type LiveModelsQuery struct {
	Key            auth.Credential
	BaseURL        string
	NativeProvider string
	PreserveNative bool
	Headers        map[string]string
	Timeout        time.Duration
	Mode           string
}

func (c *Catalog) PickerLiveModels(ctx context.Context, q LiveModelsQuery) (ids []string, native, listed bool, err error) {
	if q.Timeout <= 0 {
		q.Timeout = fullCustomProbeTimeout
	}
	live, err := c.fetchPickerLiveModels(c.scoped(ctx), q.Key, q.BaseURL, q.NativeProvider, q.PreserveNative, q.Headers, q.Timeout, q.Mode, true)
	return live.ids, live.native, live.listed, err
}

func (c *Catalog) SaveDiscoveredModels(ctx context.Context, apiURL string, ids []string, mode string, headers map[string]string, identity string) {
	c.saveDiscoveredModels(c.scoped(ctx), apiURL, ids, mode, headers, identity)
}

func (c *Catalog) EntryCredentials(ctx context.Context, entry config.ProviderEntry, keyEnvs ...string) (key auth.Credential, identity string) {
	return c.entryCredentials(c.scoped(ctx), entry, keyEnvs...)
}

func DeclaredModelIDs(models config.ProviderModels) []string { return declaredModelIDs(models) }

func ModelsAreAllowlist(entry config.ProviderEntry) bool { return modelsAreAllowlist(entry) }

func DiscoverModelsEnabled(entry config.ProviderEntry) bool {
	return scalarTruthy(entry.DiscoverModels, discoverFalseWords, false)
}

func (c *Catalog) ModelsDevModelIDs(ctx context.Context, allowNetwork bool) []string {
	registry := c.modelsDevRegistry(c.scoped(ctx), allowNetwork)
	ids := []string{}
	if registry == nil {
		return ids
	}
	for _, provider := range registry.order {
		if listed := registry.providers[provider]; listed != nil && listed.valid && listed.hasModels {
			ids = append(ids, listed.order...)
		}
	}
	return ids
}

func (c *Catalog) StaticCatalogProviders(ctx context.Context) []string {
	return c.staticCatalogProviders(c.scoped(ctx))
}

func (c *Catalog) LoadPickerContext(ctx context.Context) (PickerContext, error) {
	return c.loadPickerContext(c.scoped(ctx))
}

func (c *Catalog) PrewarmPickerCache(ctx context.Context) bool {
	ctx = c.scoped(ctx)
	st := &c.st.picker
	st.mu.Lock()
	if st.prewarmed {
		st.mu.Unlock()
		return false
	}
	st.prewarmed = true
	st.mu.Unlock()
	return c.background(ctx, func(bg context.Context) {
		picker, err := c.loadPickerContext(bg)
		if err == nil {
			_, err = c.authenticatedProviders(bg, ProvidersQuery{PickerContext: picker, ProbeCustomProviders: true})
		}
		if err != nil {
			slog.Debug("picker cache prewarm failed", "error", err)
		}
	})
}
