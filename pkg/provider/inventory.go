package provider

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// PickerConfigContext is the disk config snapshot every picker consumer reads
type PickerConfigContext struct {
	CurrentProvider   string
	CurrentModel      string
	CurrentBaseURL    string
	UserProviders     map[string]any
	CustomProviders   []any
	ExcludedProviders []string
}

// ModelsPayloadOptions mirrors the build_models_payload keyword arguments
type ModelsPayloadOptions struct {
	ExplicitOnly               bool
	IncludeUnconfigured        bool
	PickerHints                bool
	CanonicalOrder             bool
	Pricing                    bool
	PricingCacheOnly           bool
	Capabilities               bool
	Featured                   bool
	ForceFreshNousTier         bool
	Refresh                    bool
	ProbeCustomProviders       bool
	ProbeCurrentCustomProvider bool
	ForPicker                  bool
	MaxModels                  *int
	NonBlockingCatalogs        bool
	FastCustomProbe            *bool
}

// AuxPickerEntry is one rendered auxiliary picker row
type AuxPickerEntry struct {
	Slug   string
	Label  string
	Models []string
}

// Seams the inventory tests replace
var (
	inventoryListProvidersFn = ListAuthenticatedProviders
	inventoryLoadPoolFn      = func(ctx context.Context, slug string) inventoryPool {
		if pool := LoadPool(modelsPoolEnv(ctx), slug); pool != nil {
			return pool
		}
		return nil
	}
	inventoryExplicitFn       = IsProviderExplicitlyConfigured
	inventoryAnthropicOAuthFn = anthropicOAuthCredentialsPresent
	inventoryMetadataConfigFn = func(ctx context.Context) (config.Tree, error) {
		h := home.Dir(ctx)
		return config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	}
	inventoryPricingFn          = GetPricingForProvider
	inventoryCheckNousTierFn    = CheckNousFreeTier
	inventoryPartitionFn        = PartitionNousModelsByTier
	inventoryCachedNousTierFn   = GetCachedNousFreeTier
	inventoryNousPickerUsageFn  = nousPickerUsageIn
	inventoryStagedModelsFn     = stagedModelIDs
	inventoryStateEndpointFn    = stateEndpoint
	inventoryNousCapsFn         = NousModelReasoningCapabilities
	inventoryOpenRouterCapsFn   = OpenRouterModelReasoningCapabilities
	inventoryWarmNousCapsFn     = WarmNousReasoningCapsAsync
	inventoryWarmOpenRouterFn   = WarmOpenRouterReasoningCapsAsync
	inventoryRefreshUsageFn     = refreshAccountUsageAsync
	inventoryRefreshEntriesFn   = refreshAccountUsageEntriesAsync
	inventoryPricingScopeFn     = PricingCacheScope
	inventoryApplyCapsFn        = applyCapabilities
	inventoryApplyFeaturedFn    = applyFeatured
	inventoryStripOverlapsFn    = stripAggregatorOverlaps
	inventoryPrewarmFn          = prewarmPricingAsync
	inventoryExhaustedPercent   = 100.0
	inventoryFeaturedPerLab     = 5
	inventoryPricingPrewarmMu   sync.Mutex
	inventoryPricingPrewarmRuns = map[string]chan struct{}{}
)

// Credential pool view the limit and usage decorators read
type inventoryPool interface {
	Entries() []PooledCredential
	LimitState(models []string) *LimitState
}

// Copy with the non empty overrides applied so blank agent fields never clobber disk values
func (pc PickerConfigContext) WithOverrides(currentProvider, currentModel, currentBaseURL string) PickerConfigContext {
	if currentProvider != "" {
		pc.CurrentProvider = currentProvider
	}
	if currentModel != "" {
		pc.CurrentModel = currentModel
	}
	if currentBaseURL != "" {
		pc.CurrentBaseURL = currentBaseURL
	}
	return pc
}

// Disk config snapshot of the current model, providers and catalog exclusions
func loadPickerContext(ctx context.Context) (PickerConfigContext, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return PickerConfigContext{}, err
	}
	cfg := map[string]any(tree)
	var pc PickerConfigContext
	modelCfg, has := cfg["model"]
	if !has {
		modelCfg = map[string]any{}
	}
	if m, ok := modelCfg.(map[string]any); ok {
		def, hasDefault := m["default"]
		if !hasDefault {
			def = pyGetAny(m, "name", "")
		}
		pc.CurrentModel = pyOrStr(def)
		pc.CurrentProvider = CoerceProviderID(pyGetAny(m, "provider", ""))
		pc.CurrentBaseURL = pyOrStr(pyGetAny(m, "base_url", ""))
	} else {
		pc.CurrentModel = pyOrStr(modelCfg)
	}
	catalog, has := cfg["model_catalog"]
	if !has {
		catalog = map[string]any{}
	}
	catalogMap, ok := catalog.(map[string]any)
	if !ok {
		return PickerConfigContext{}, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(catalog))
	}
	if excluded, ok := catalogMap["excluded_providers"].([]any); ok {
		for _, p := range excluded {
			if p != nil {
				pc.ExcludedProviders = append(pc.ExcludedProviders, pyStr(p))
			}
		}
	}
	pc.UserProviders = StringifyProviderMap(cfg["providers"])
	for _, entry := range CompatibleCustomProviders(cfg) {
		pc.CustomProviders = append(pc.CustomProviders, entry)
	}
	return pc, nil
}

// Lowercased trimmed slug of a row
func inventorySlug(row map[string]any) string {
	return strings.ToLower(strings.TrimSpace(pyOrStr(row["slug"])))
}

// Rows minus every row carrying the slug
func withoutSlug(rows []map[string]any, slug string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		if inventorySlug(r) != slug {
			out = append(out, r)
		}
	}
	return out
}

// Model ids of a row as strings in listing order
func inventoryModels(row map[string]any) []string {
	switch v := row["models"].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, m := range v {
			out = append(out, pyStr(m))
		}
		return out
	}
	return nil
}

// Payload of provider rows with the current model and provider every picker surface consumes
func buildModelsPayload(ctx context.Context, pc PickerConfigContext, opts ModelsPayloadOptions) (map[string]any, error) {
	rows, err := buildModelsRows(ctx, pc, opts)
	if err != nil {
		return nil, err
	}
	providers := make([]any, len(rows))
	for i, r := range rows {
		providers[i] = r
	}
	DropUnofferableModelIDs(providers)
	payload := map[string]any{"providers": providers, "model": pc.CurrentModel, "provider": pc.CurrentProvider}
	config.SetMappingKeys(payload, []string{"providers", "model", "provider"})
	return payload, nil
}

// Decorated provider rows before the unofferable model filter
func buildModelsRows(ctx context.Context, pc PickerConfigContext, opts ModelsPayloadOptions) ([]map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	excluded := pc.ExcludedProviders
	if excluded == nil {
		excluded = []string{}
	}
	rows, err := inventoryListProvidersFn(ctx, AuthenticatedProvidersOptions{
		CurrentProvider: pc.CurrentProvider, CurrentBaseURL: pc.CurrentBaseURL, CurrentModel: pc.CurrentModel,
		UserProviders: pc.UserProviders, CustomProviders: pc.CustomProviders, ForceFreshNousTier: opts.ForceFreshNousTier,
		MaxModels: opts.MaxModels, Refresh: opts.Refresh, SkipCustomProviderProbe: !opts.ProbeCustomProviders,
		ProbeCurrentCustomProvider: opts.ProbeCurrentCustomProvider, ForPicker: opts.ForPicker,
		ExcludedProviders: excluded, NonBlockingCatalogs: opts.NonBlockingCatalogs, FastCustomProbe: opts.FastCustomProbe,
	})
	if err != nil {
		return nil, err
	}
	localRow := localRuntimeRow(ctx, pc)
	if localRow != nil {
		rows = append(withoutSlug(rows, "llamacpp"), localRow)
		if pyTruthy(localRow["is_current"]) {
			staged := map[string]bool{}
			for _, m := range inventoryModels(localRow) {
				staged[m] = true
			}
			kept := rows[:0:0]
			for _, r := range rows {
				if !isManagedCustomRow(r, staged) {
					kept = append(kept, r)
				}
			}
			rows = kept
		}
	}
	if moa := moaProviderRow(ctx, pc.CurrentProvider); moa != nil {
		rows = append([]map[string]any{moa}, withoutSlug(rows, "moa")...)
	}
	if opts.ExplicitOnly {
		rows = filterExplicitProviderRows(ctx, rows, pc)
		localOwnsCurrent := localRow != nil && pyTruthy(localRow["is_current"]) && strings.ToLower(pc.CurrentProvider) == "custom"
		if !localOwnsCurrent {
			rows = append(rows, appendUnconfiguredRows(ctx, rows, pc, true)...)
		}
	}
	inventoryStripOverlapsFn(ctx, rows)
	if opts.IncludeUnconfigured {
		rows = append(rows, withoutSlug(appendUnconfiguredRows(ctx, rows, pc, false), "moa")...)
	}
	if opts.PickerHints {
		applyPickerHints(ctx, rows)
	}
	if opts.CanonicalOrder {
		rows = reorderCanonical(rows)
	}
	if opts.Pricing {
		applyPricing(ctx, rows, opts.ForceFreshNousTier, opts.PricingCacheOnly)
	}
	var metadataConfig map[string]any
	if opts.Capabilities || opts.Featured {
		if tree, err := inventoryMetadataConfigFn(ctx); err == nil {
			metadataConfig = map[string]any(tree)
		}
	}
	if opts.Capabilities {
		if err := inventoryApplyCapsFn(ctx, rows, metadataConfig); err != nil {
			return nil, err
		}
	}
	if opts.Featured {
		inventoryApplyFeaturedFn(ctx, rows, metadataConfig)
	}
	applyCustomAliases(rows)
	return rows, nil
}

// A custom row whose models all live on the managed local server duplicates the Local row
func isManagedCustomRow(row map[string]any, staged map[string]bool) bool {
	models := inventoryModels(row)
	if inventorySlug(row) != "custom" || len(models) == 0 {
		return false
	}
	for _, m := range models {
		if !staged[m] {
			return false
		}
	}
	return true
}

// Drops from routing aggregator rows the models a user defined provider also serves
func stripAggregatorOverlaps(ctx context.Context, rows []map[string]any) {
	builtinAggregators := map[string]bool{}
	for _, row := range rows {
		if !pyTruthy(row["is_user_defined"]) && isRoutingAggregatorCtx(ctx, inventorySlug(row)) {
			builtinAggregators[inventorySlug(row)] = true
		}
	}
	duplicatesBuiltinAggregator := func(row map[string]any) bool {
		rowSlug := inventorySlug(row)
		if suffix, ok := strings.CutPrefix(rowSlug, "custom:"); ok && suffix != "" && builtinAggregators[suffix] {
			return true
		}
		inferred := inferProviderFromBaseURL(pyOrStr(row["api_url"]))
		return inferred != "" && builtinAggregators[inferred]
	}
	userModels := map[string]bool{}
	for _, row := range rows {
		if !pyTruthy(row["is_user_defined"]) {
			continue
		}
		if len(builtinAggregators) > 0 && duplicatesBuiltinAggregator(row) {
			continue
		}
		for _, m := range inventoryModels(row) {
			userModels[strings.ToLower(m)] = true
		}
	}
	if len(userModels) == 0 {
		return
	}
	for _, row := range rows {
		if pyTruthy(row["is_user_defined"]) || !isRoutingAggregatorCtx(ctx, pyStr(pyGetAny(row, "slug", ""))) {
			continue
		}
		original := inventoryModels(row)
		filtered := make([]string, 0, len(original))
		for _, m := range original {
			if !userModels[strings.ToLower(m)] {
				filtered = append(filtered, m)
			}
		}
		if len(filtered) < len(original) {
			row["models"] = stringsToAny(filtered)
			row["total_models"] = len(filtered)
		}
	}
}

// BuildModelOptionsPayload is the shared dashboard, API server and TUI payload with limits and usage attached
func BuildModelOptionsPayload(ctx context.Context, pc PickerConfigContext, explicitOnly, includeUnconfigured, refresh bool) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fastCustomProbe := false
	payload, err := buildModelsPayload(ctx, pc, ModelsPayloadOptions{
		ExplicitOnly: explicitOnly, IncludeUnconfigured: includeUnconfigured, PickerHints: true, CanonicalOrder: true,
		Pricing: true, PricingCacheOnly: !refresh, Capabilities: true, Featured: true, ForPicker: true,
		FastCustomProbe: &fastCustomProbe, Refresh: refresh, ProbeCustomProviders: refresh,
		ProbeCurrentCustomProvider: !refresh, NonBlockingCatalogs: !refresh,
	})
	if err != nil {
		return nil, err
	}
	rows := payloadRows(payload)
	if err := applyLimits(ctx, rows); err != nil {
		return nil, err
	}
	if err := applyUsage(ctx, rows); err != nil {
		return nil, err
	}
	if !refresh {
		inventoryPrewarmFn(ctx, rows, pc.CurrentProvider, pc.CurrentBaseURL)
	}
	return payload, nil
}

// Row maps of a payload's providers list
func payloadRows(payload map[string]any) []map[string]any {
	list, _ := payload["providers"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// Python datetime.fromtimestamp(epoch, UTC).isoformat text
func inventoryISOFromEpoch(epoch float64) string {
	frac, whole := math.Modf(epoch)
	us := math.RoundToEven(frac * 1e6)
	if us >= 1e6 {
		whole++
		us -= 1e6
	} else if us < 0 {
		whole--
		us += 1e6
	}
	return inventoryISOTime(time.Unix(int64(whole), int64(us)*1000).UTC())
}

// Python aware datetime isoformat text with microseconds only when non zero
func inventoryISOTime(t time.Time) string {
	text := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text + t.Format("-07:00")
}

// Attaches the pool limit to rows whose credential pool is rate limited
func applyLimits(ctx context.Context, rows []map[string]any) error {
	pools, err := modelsPoolEnv(ctx).Store.ReadCredentialPools()
	if err != nil {
		return err
	}
	for _, row := range rows {
		slug := pyOrStr(row["slug"])
		if len(pools[slug]) == 0 || pyTruthy(row["is_user_defined"]) || pyTruthy(row["plan_usage"]) {
			continue
		}
		pool := inventoryLoadPoolFn(ctx, slug)
		if pool == nil {
			modelsLog.Debug("Pool limit read failed", "provider", slug)
			continue
		}
		state := pool.LimitState(inventoryModels(row))
		if state == nil {
			continue
		}
		if state.Scope == "account" {
			row["limit"] = map[string]any{"scope": "account", "resets_at": inventoryISOFromEpoch(state.ResetsAt)}
			continue
		}
		models := map[string]any{}
		for m, at := range state.Models {
			models[m] = inventoryISOFromEpoch(at)
		}
		row["limit"] = map[string]any{"scope": "models", "models": models}
	}
	return nil
}

// Wire windows of a usage snapshot, only those with a known percentage
func wireUsageWindows(snapshot *AccountUsageSnapshot) []any {
	out := []any{}
	if snapshot == nil {
		return out
	}
	for _, w := range snapshot.Windows {
		if w.UsedPercent == nil {
			continue
		}
		var resetsAt any
		if w.ResetAt != nil {
			resetsAt = inventoryISOTime(*w.ResetAt)
		}
		out = append(out, map[string]any{"label": w.Label, "used_percent": *w.UsedPercent, "resets_at": resetsAt, "scope": w.Scope})
	}
	return out
}

// Latest recovery time of an exhausted account, a live cooldown included, nil when unknown
func accountResetsAt(snapshot *AccountUsageSnapshot, liveCooldownUntil *float64) any {
	var resets []string
	if liveCooldownUntil != nil && *liveCooldownUntil != 0 {
		resets = append(resets, inventoryISOFromEpoch(*liveCooldownUntil))
	}
	if snapshot != nil {
		for _, w := range snapshot.Windows {
			if w.Scope != "account" || w.UsedPercent == nil || w.ResetAt == nil {
				continue
			}
			if *w.UsedPercent >= inventoryExhaustedPercent {
				resets = append(resets, inventoryISOTime(*w.ResetAt))
			}
		}
	}
	if len(resets) == 0 {
		return nil
	}
	latest := resets[0]
	for _, r := range resets[1:] {
		if r > latest {
			latest = r
		}
	}
	return latest
}

// Attaches cached subscription usage per signed in row and queues background refreshes
func applyUsage(ctx context.Context, rows []map[string]any) error {
	pools, err := modelsPoolEnv(ctx).Store.ReadCredentialPools()
	if err != nil {
		return err
	}
	wanted := []string{}
	entryRequests := []map[string]any{}
	for _, row := range rows {
		slug := pyOrStr(row["slug"])
		if slug == "" || pyTruthy(row["is_user_defined"]) || row["authenticated"] == false {
			continue
		}
		supportsUsage := hasAccountUsage(ctx, slug)
		if len(pools[slug]) < 2 {
			if !supportsUsage {
				continue
			}
			wanted = append(wanted, slug)
			snapshot := CachedAccountUsage(ctx, slug, "", "")
			if windows := wireUsageWindows(snapshot); len(windows) > 0 && !snapshotIsStale(snapshot) {
				row["usage"] = map[string]any{"windows": windows}
			}
			continue
		}
		if supportsUsage {
			wanted = append(wanted, slug)
		}
		if accounts := poolUsageAccounts(ctx, slug, &entryRequests); accounts != nil {
			row["usage"] = map[string]any{"accounts": accounts}
		}
	}
	inventoryRefreshUsageFn(ctx, wanted)
	inventoryRefreshEntriesFn(ctx, entryRequests)
	return nil
}

var accountStateRank = map[string]int{"limited": 3, "ready": 2, "unknown": 1, "unavailable": 0}

// Per account usage rows of one pooled provider deduped by host and identity, nil when the pool is unreadable
func poolUsageAccounts(ctx context.Context, slug string, entryRequests *[]map[string]any) []any {
	pool := inventoryLoadPoolFn(ctx, slug)
	if pool == nil {
		modelsLog.Debug("Pool usage read failed", "provider", slug)
		return nil
	}
	entries := pool.Entries()
	if len(entries) == 0 {
		return nil
	}
	env := modelsPoolEnv(ctx)
	live := 0
	for _, e := range entries {
		if e.LastStatus != StatusDead {
			live++
		}
	}
	sole := live <= 1
	supportsUsage := hasAccountUsage(ctx, slug)
	now := nowEpoch()
	var accounts []any
	seen := map[[3]string]map[string]any{}
	for _, entry := range entries {
		identityID := identityIDFor(slug, entry)
		dedupeKey := [3]string{slug, poolEntryHost(env, slug, entry), identityID}
		isDead := entry.LastStatus == StatusDead
		var snapshot *AccountUsageSnapshot
		if supportsUsage && !isDead {
			snapshot = CachedAccountUsage(ctx, slug, identityID, "")
		}
		windows := []any{}
		if !snapshotIsStale(snapshot) {
			windows = wireUsageWindows(snapshot)
		}
		var liveCooldown *float64
		if !isDead {
			if until, ok := exhaustedUntil(entry, sole); ok && until != 0 && until > now {
				liveCooldown = &until
			}
		}
		quotaExhausted := false
		for _, w := range windows {
			wm := w.(map[string]any)
			if pct, ok := wm["used_percent"].(float64); ok && wm["scope"] == "account" && pct >= inventoryExhaustedPercent {
				quotaExhausted = true
			}
		}
		state := "ready"
		switch {
		case isDead:
			state = "unavailable"
		case liveCooldown != nil || quotaExhausted:
			state = "limited"
		case snapshot == nil || snapshotIsStale(snapshot) || len(windows) == 0:
			state = "unknown"
		}
		accountRow := map[string]any{"id": identityID, "label": entry.Label, "windows": windows, "state": state,
			"resets_at": accountResetsAt(snapshot, liveCooldown)}
		if existing, dup := seen[dedupeKey]; dup {
			if accountStateRank[state] > accountStateRank[pyStr(existing["state"])] {
				for i, a := range accounts {
					if reflect.DeepEqual(a, existing) {
						accounts[i] = accountRow
						break
					}
				}
				seen[dedupeKey] = accountRow
			}
			continue
		}
		seen[dedupeKey] = accountRow
		accounts = append(accounts, accountRow)
		if supportsUsage && !isDead {
			*entryRequests = append(*entryRequests, map[string]any{"provider": slug, "identity_id": identityID,
				"base_url": poolEntryRouteBaseURL(env, slug, entry), "api_key": entry.RuntimeAPIKey()})
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	return accounts
}

// Normalized route host of one pool entry for account dedupe scoping
func poolEntryHost(env PoolEnv, slug string, entry PooledCredential) string {
	if slug == "openai-codex" {
		return strings.TrimRight(strings.TrimSpace(codexPoolRouteBaseURL(env, entry.RuntimeBaseURL())), "/")
	}
	return strings.TrimRight(strings.TrimSpace(entry.RuntimeBaseURL()), "/")
}

// Base URL a usage fetch for the entry targets, nil when the entry has none
func poolEntryRouteBaseURL(env PoolEnv, slug string, entry PooledCredential) any {
	if slug == "openai-codex" {
		return codexPoolRouteBaseURL(env, entry.RuntimeBaseURL())
	}
	if base := entry.RuntimeBaseURL(); base != "" {
		return base
	}
	return nil
}

// BuildAuxPickerRows lists provider rows for auxiliary task pickers with moa removed
func BuildAuxPickerRows(ctx context.Context, currentProvider, currentModel, currentBaseURL string, maxModels *int) ([]map[string]any, error) {
	pc, err := loadPickerContext(ctx)
	if err != nil {
		return nil, err
	}
	pc = pc.WithOverrides(currentProvider, currentModel, currentBaseURL)
	payload, err := buildModelsPayload(ctx, pc, ModelsPayloadOptions{ForPicker: true, ProbeCustomProviders: false,
		ProbeCurrentCustomProvider: true, MaxModels: maxModels})
	if err != nil {
		return nil, err
	}
	return withoutSlug(payloadRows(payload), "moa"), nil
}

// FormatAuxPickerEntries renders rows as slug, label with model count and current marker, and models
func FormatAuxPickerEntries(rows []map[string]any, currentProvider, currentBaseURL string) []AuxPickerEntry {
	entries := make([]AuxPickerEntry, 0, len(rows))
	currentSlug := strings.ToLower(strings.TrimSpace(currentProvider))
	hasBaseURL := strings.TrimSpace(currentBaseURL) != ""
	for _, row := range rows {
		slug := pyOrStr(row["slug"])
		name := slug
		if pyTruthy(row["name"]) {
			name = pyStr(row["name"])
		}
		models := inventoryModels(row)
		total := row["total_models"]
		if !pyTruthy(total) {
			total = len(models)
		}
		modelHint := ""
		if pyTruthy(total) {
			modelHint = " — " + pyStr(total) + " models"
		}
		marker := ""
		if strings.ToLower(slug) == currentSlug && currentSlug != "" && !hasBaseURL {
			marker = "  ← current"
		}
		if models == nil {
			models = []string{}
		}
		entries = append(entries, AuxPickerEntry{Slug: slug, Label: name + modelHint + marker, Models: models})
	}
	return entries
}

// Cache only per model reasoning capability reader of an aggregator that publishes one, warming it first
func reasoningCatalogReader(ctx context.Context, slug string) func(model string) (ReasoningCapabilities, bool) {
	switch slug {
	case "nous":
		inventoryWarmNousCapsFn(ctx)
		return func(model string) (ReasoningCapabilities, bool) { return inventoryNousCapsFn(ctx, model, 0, false) }
	case "openrouter":
		inventoryWarmOpenRouterFn(ctx)
		return func(model string) (ReasoningCapabilities, bool) {
			return inventoryOpenRouterCapsFn(ctx, model, 0, false)
		}
	}
	return nil
}

// Attaches the fast, reasoning, ultrafast and daybreak capability map of every model per row
func applyCapabilities(ctx context.Context, rows []map[string]any, metadataConfig map[string]any) error {
	h := home.Dir(ctx)
	modelCfg, _ := metadataConfig["model"].(map[string]any)
	appServer := modelCfg != nil && strings.ToLower(strings.TrimSpace(pyOrStr(modelCfg["openai_runtime"]))) == "codex_app_server"
	for _, row := range rows {
		slug := pyOrStr(row["slug"])
		baseURL := pyOrStr(row["api_url"])
		caps := map[string]any{}
		readReasoningCatalog := reasoningCatalogReader(ctx, strings.ToLower(slug))
		for _, model := range inventoryModels(row) {
			reasoning := true
			if slug != "" {
				var meta ModelCapabilities
				var ok bool
				if metadataConfig != nil {
					meta, ok = modelsDevCapabilitiesIn(h, slug, model, false, metadataConfig)
				} else {
					meta, ok = ModelsDevCapabilities(ctx, slug, model, false)
				}
				if ok && meta.SupportsReasoning != nil {
					reasoning = *meta.SupportsReasoning
				}
			}
			fast := ResolveFastModeOverrides(model, slug, baseURL, "") != nil
			entry := map[string]any{"fast": fast, "reasoning": reasoning}
			if fast && ModelSupportsUltrafast(model) {
				offered, err := UltrafastOffered(model, slug, baseURL, "")
				if err != nil {
					return err
				}
				if offered {
					entry["ultrafast"] = true
				}
			}
			if !appServer {
				offered, err := DaybreakOffered(model, slug, baseURL, "")
				if err != nil {
					return err
				}
				if offered {
					entry["daybreak"] = true
				}
			}
			if reasoning && readReasoningCatalog != nil {
				if detail, ok := readReasoningCatalog(model); ok && !detail.SupportsReasoning {
					entry["reasoning"] = false
				} else if ok {
					entry["can_disable_reasoning"] = !detail.Mandatory
				}
			}
			caps[model] = entry
		}
		row["capabilities"] = caps
	}
	return nil
}

type featuredCandidate struct {
	pos   int
	date  string
	model string
}

// Attaches the newest models per lab shortlist to multi lab routing aggregator rows
func applyFeatured(ctx context.Context, rows []map[string]any, metadataConfig map[string]any) {
	h := home.Dir(ctx)
	modelInfo := func(provider, model string) (ModelsDevEntry, bool) {
		if metadataConfig != nil {
			return modelsDevModelInfoIn(h, provider, model, false, metadataConfig)
		}
		return ModelsDevModelInfo(ctx, provider, model, false)
	}
	for _, row := range rows {
		slug := strings.ToLower(strings.TrimSpace(pyOrStr(row["slug"])))
		models := inventoryModels(row)
		if pyTruthy(row["is_user_defined"]) || !isRoutingAggregatorCtx(ctx, slug) {
			row["featured_models"] = []any{}
			continue
		}
		var labs []string
		byLab := map[string][]featuredCandidate{}
		for pos, model := range models {
			lab, _, hasSlash := strings.Cut(model, "/")
			if !hasSlash || lab == "" {
				labs, byLab = nil, map[string][]featuredCandidate{}
				break
			}
			date := ""
			info, ok := modelInfo(slug, model)
			if !ok {
				info, ok = modelInfo("openrouter", model)
			}
			if ok {
				date = info.ReleaseDate
			}
			if _, known := byLab[lab]; !known {
				labs = append(labs, lab)
			}
			byLab[lab] = append(byLab[lab], featuredCandidate{pos: pos, date: date, model: model})
		}
		if len(labs) < 2 {
			row["featured_models"] = []any{}
			continue
		}
		var featured []string
		for _, lab := range labs {
			ranked := append([]featuredCandidate(nil), byLab[lab]...)
			sort.SliceStable(ranked, func(i, j int) bool {
				if ranked[i].date != ranked[j].date {
					return ranked[i].date > ranked[j].date
				}
				return ranked[i].pos < ranked[j].pos
			})
			for _, c := range ranked[:min(len(ranked), inventoryFeaturedPerLab)] {
				featured = append(featured, c.model)
			}
		}
		order := map[string]int{}
		for i, m := range models {
			order[m] = i
		}
		sort.SliceStable(featured, func(i, j int) bool { return order[featured[i]] < order[featured[j]] })
		row["featured_models"] = stringsToAny(featured)
	}
}

// Attaches every accepted spelling of a user defined provider so GUI pickers can match it
func applyCustomAliases(rows []map[string]any) {
	for _, row := range rows {
		if !pyTruthy(row["is_user_defined"]) {
			continue
		}
		set := CustomProviderAliases(pyStr(pyGetAny(row, "name", "")), pyStr(pyGetAny(row, "slug", "")))
		aliases := make([]string, 0, len(set))
		for a := range set {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		row["aliases"] = stringsToAny(aliases)
	}
}

// Auth type and first key env var of a canonical provider, api_key and empty when unregistered
func providerAuthHint(ctx context.Context, slug string) (string, string) {
	cfg := authRegistryGet(ctx, slug)
	if cfg == nil {
		return "api_key", ""
	}
	keyEnv := ""
	if len(cfg.KeyEnv) > 0 {
		keyEnv = cfg.KeyEnv[0]
	}
	return cfg.AuthType, keyEnv
}

// Base picker row with extra fields merged over it
func inventoryRow(slug, name string, isCurrent bool, extra map[string]any) map[string]any {
	row := map[string]any{"slug": slug, "name": name, "is_current": isCurrent, "is_user_defined": false}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// Picker row of a canonical provider under its catalog label
func canonicalRow(entry CanonicalProviderEntry, cur string, extra map[string]any) map[string]any {
	label := CatalogProviderLabel(entry.Slug)
	if label == "" {
		label = entry.Label
	}
	return inventoryRow(entry.Slug, label, strings.ToLower(entry.Slug) == cur, extra)
}

// Setup skeletons for canonical providers missing from rows, the current one keeping its saved model
func appendUnconfiguredRows(ctx context.Context, rows []map[string]any, pc PickerConfigContext, currentOnly bool) []map[string]any {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[strings.ToLower(pyStr(r["slug"]))] = true
	}
	cur := strings.ToLower(pc.CurrentProvider)
	curModel := strings.TrimSpace(pc.CurrentModel)
	excluded := map[string]bool{}
	for _, p := range pc.ExcludedProviders {
		if p != "" {
			excluded[strings.ToLower(strings.TrimSpace(p))] = true
		}
	}
	namesFor := map[string][]string{}
	for alias, canon := range CatalogProviderAliases() {
		c := strings.ToLower(canon)
		if _, ok := namesFor[c]; !ok {
			namesFor[c] = []string{c}
		}
		namesFor[c] = append(namesFor[c], strings.ToLower(alias))
	}
	extras := []map[string]any{}
	for _, entry := range ListedCanonicalProviders() {
		slug := strings.ToLower(entry.Slug)
		if seen[slug] {
			continue
		}
		names, ok := namesFor[slug]
		if !ok {
			names = []string{slug}
		}
		hidden := false
		for _, n := range names {
			hidden = hidden || excluded[n]
		}
		if hidden || (currentOnly && slug != cur) {
			continue
		}
		if slug != cur {
			extras = append(extras, canonicalRow(entry, cur, map[string]any{"models": []any{}, "total_models": 0, "source": "canonical"}))
			continue
		}
		savedModel := curModel
		if modelRequiresAccountDiscovery(entry.Slug, curModel) {
			savedModel = ""
		}
		authType, keyEnv := providerAuthHint(ctx, entry.Slug)
		tail := "Showing the saved model only."
		if curModel != "" && savedModel == "" {
			tail = "Astra requires successful account-scoped model discovery."
		}
		warning := "Configured provider is not authenticated; run `icebath model` to reactivate. " + tail
		if authType == "api_key" && keyEnv != "" {
			warning = "Configured provider missing usable credentials; paste " + keyEnv + " to reactivate. " + tail
		}
		models, total := []any{}, 0
		if savedModel != "" {
			models, total = []any{savedModel}, 1
		}
		extras = append(extras, canonicalRow(entry, cur, map[string]any{"models": models, "total_models": total,
			"source": "configured-current", "authenticated": false, "auth_type": authType, "key_env": keyEnv, "warning": warning}))
	}
	return extras
}

// True when Anthropic was authenticated through OAuth, device flow, Claude Code login or a pooled OAuth entry
func anthropicOAuthCredentialsPresent(ctx context.Context) bool {
	env := modelsPoolEnv(ctx)
	for _, read := range []func(PoolEnv) map[string]any{ReadHermesOAuthCredentials, ReadClaudeCodeCredentials} {
		if pyTruthy(read(env)["accessToken"]) {
			return true
		}
	}
	entries, err := env.Store.ReadCredentialPool("anthropic")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry["auth_type"] == AuthTypeOAuth && strings.TrimSpace(pyOrStr(entry["access_token"])) != "" {
			return true
		}
	}
	return false
}

// Rows backed by explicit user configuration, hiding ambient credentials discovered elsewhere
func filterExplicitProviderRows(ctx context.Context, rows []map[string]any, pc PickerConfigContext) []map[string]any {
	currentSlug := strings.ToLower(strings.TrimSpace(pc.CurrentProvider))
	env := modelsPoolEnv(ctx)
	isExplicit := func(row map[string]any, slug string) bool {
		if pyTruthy(row["is_user_defined"]) || (currentSlug != "" && slug == currentSlug) || row["source"] == "local-runtime" {
			return true
		}
		if slug == "moa" {
			return rawConfigHasEnabledMoAPreset(ctx)
		}
		return (slug == "anthropic" && inventoryAnthropicOAuthFn(ctx)) || externalProcessSignedIn(ctx, slug) ||
			inventoryExplicitFn(env, slug)
	}
	out := []map[string]any{}
	for _, row := range rows {
		if slug := strings.ToLower(strings.TrimSpace(pyStr(pyGetAny(row, "slug", "")))); slug != "" && isExplicit(row, slug) {
			out = append(out, row)
		}
	}
	return out
}

// True when an external process provider has verified CLI credentials
func externalProcessSignedIn(ctx context.Context, slug string) bool {
	pconfig := authRegistryGet(ctx, slug)
	return pconfig != nil && pconfig.AuthType == "external_process" &&
		pyTruthy(GetExternalProcessProviderStatus(modelsPoolEnv(ctx), slug)["auth_verified"])
}

var legacyMoAKeys = []string{"reference_models", "aggregator", "reference_temperature", "aggregator_temperature",
	"max_tokens", "reference_max_tokens", "fanout"}

// True when the raw user config enables a MoA preset or saves the legacy flat MoA config
func rawConfigHasEnabledMoAPreset(ctx context.Context) bool {
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return false
	}
	moa, ok := raw["moa"].(map[string]any)
	if !ok {
		return false
	}
	if presets, ok := moa["presets"].(map[string]any); ok {
		for name, preset := range presets {
			if strings.TrimSpace(name) == "" {
				continue
			}
			p, isMap := preset.(map[string]any)
			if !isMap || pyTruthy(pyGetAny(p, "enabled", true)) {
				return true
			}
		}
		return false
	}
	for _, key := range legacyMoAKeys {
		if _, has := moa[key]; has {
			return pyTruthy(pyGetAny(moa, "enabled", true))
		}
	}
	return false
}

// Adds authenticated, auth_type, key_env and warning to rows lacking them
func applyPickerHints(ctx context.Context, rows []map[string]any) {
	for _, row := range rows {
		if _, has := row["authenticated"]; has {
			continue
		}
		isSkeleton := row["source"] == "canonical" && !pyTruthy(row["models"])
		row["authenticated"] = !isSkeleton
		if !isSkeleton || pyTruthy(row["is_user_defined"]) {
			continue
		}
		authType, keyEnv := providerAuthHint(ctx, pyStr(row["slug"]))
		row["auth_type"] = authType
		row["key_env"] = keyEnv
		if authType == "api_key" && keyEnv != "" {
			row["warning"] = "paste " + keyEnv + " to activate"
		} else {
			row["warning"] = "run `icebath model` to configure (" + authType + ")"
		}
	}
}

// Canonical slugs in canonical provider order with the other rows after
func reorderCanonical(rows []map[string]any) []map[string]any {
	order := map[string]int{}
	for i, e := range CanonicalProviders() {
		order[e.Slug] = i
	}
	var canon, extras []map[string]any
	for _, r := range rows {
		if _, ok := order[pyStr(r["slug"])]; ok {
			canon = append(canon, r)
		} else {
			extras = append(extras, r)
		}
	}
	sort.SliceStable(canon, func(i, j int) bool { return order[pyStr(canon[i]["slug"])] < order[pyStr(canon[j]["slug"])] })
	return append(append([]map[string]any{}, canon...), extras...)
}

// Stamps the Nous window plan usage on the row and feeds the usage chip, nil without a window plan
func attachNousPlanUsage(ctx context.Context, row map[string]any, warm bool) *PickerUsage {
	usage := inventoryNousPickerUsageFn(ctx, warm, !warm)
	if usage == nil {
		return nil
	}
	row["plan_usage"] = usage.ToPayload()
	if usage.PausedReason != "" || pyTruthy(row["usage"]) {
		return usage
	}
	for _, g := range usage.Gauges {
		if g.LeftFraction <= 0.0 {
			return usage
		}
	}
	windows := []any{}
	for _, g := range usage.Gauges {
		if g.ResetsAtMs != nil && *g.ResetsAtMs != 0 {
			windows = append(windows, planUsageWindow(g))
		}
	}
	if len(windows) > 0 {
		row["usage"] = map[string]any{"windows": windows}
	}
	return usage
}

// One window plan allowance in the provider usage window shape
func planUsageWindow(g UsageGauge) map[string]any {
	title := g.Title
	if title == "" {
		title = g.Label
	}
	scope := "model"
	if g.Scope == ScopeAll {
		scope = "account"
	}
	var ms int64
	if g.ResetsAtMs != nil {
		ms = *g.ResetsAtMs
	}
	return map[string]any{
		"label":        GaugeTitle(WindowGauge{ID: g.Label, Scope: g.Scope, Label: title, UsedBp: 0}),
		"used_percent": pyRoundPlaces(g.UsedFraction()*100.0, 2),
		"resets_at":    inventoryISOFromEpoch(float64(ms) / 1000),
		"scope":        scope,
	}
}

// Attaches per model pricing and for Nous the free tier entitlement and plan usage
func applyPricing(ctx context.Context, rows []map[string]any, forceFreshNousTier, cachedOnly bool) {
	var nousFreeTier *bool
	for _, row := range rows {
		slug := strings.ToLower(pyStr(pyGetAny(row, "slug", "")))
		models := inventoryModels(row)
		if len(models) == 0 || pyTruthy(row["free_tier_row"]) {
			continue
		}
		baseURL := ""
		if strings.HasPrefix(slug, "custom:") {
			baseURL = pyOrStr(row["api_url"])
		}
		rawPricing := inventoryPricingFn(ctx, slug, baseURL, false, cachedOnly)
		var cachedNousTier *bool
		var planUsage *PickerUsage
		if slug == "nous" {
			planUsage = attachNousPlanUsage(ctx, row, !cachedOnly)
		}
		if slug == "nous" && cachedOnly {
			tier, known := inventoryCachedNousTierFn(ctx)
			if !known {
				row["free_tier_pending"] = true
				row["unavailable_models"] = stringsToAny(models)
				if !pyTruthy(row["warning"]) {
					row["warning"] = "Checking Nous plan entitlement… models unlock on the next picker open or refresh."
				}
				continue
			}
			cachedNousTier = &tier
		}
		if len(rawPricing) == 0 {
			if slug == "nous" {
				free := cachedNousTier != nil && *cachedNousTier
				row["free_tier"] = free
				row["pricing_pending"] = true
				row["unavailable_models"] = []any{}
				if free {
					row["unavailable_models"] = stringsToAny(models)
				}
			}
			continue
		}
		formatted := map[string]any{}
		for _, mid := range models {
			p := rawPricing[mid]
			if len(p) == 0 {
				continue
			}
			formatted[mid] = formatModelPricing(slug, p)
		}
		if len(formatted) > 0 {
			row["pricing"] = formatted
		}
		if slug != "nous" {
			continue
		}
		if nousFreeTier == nil {
			tier := false
			if cachedOnly {
				tier = cachedNousTier != nil && *cachedNousTier
			} else {
				tier = inventoryCheckNousTierFn(ctx, forceFreshNousTier, false)
			}
			nousFreeTier = &tier
		}
		row["free_tier"] = *nousFreeTier
		row["unavailable_models"] = []any{}
		if *nousFreeTier {
			_, unavailable := inventoryPartitionFn(models, rawPricing, true)
			row["unavailable_models"] = stringsToAny(unavailable)
		}
		if planUsage != nil {
			row["usage_levels"] = UsageLevels(models, PricingTableAsAny(rawPricing))
			perModel := map[string]any{}
			for _, m := range models {
				if p, ok := rawPricing[m]; ok {
					perModel[m] = p
				} else {
					perModel[m] = nil
				}
			}
			row["plan_usage"] = planUsage.WithPricing(perModel).ToPayload()
			delete(row, "pricing")
		}
	}
}

// Formatted input, output, cache and free flags of one model price, with Nous sale chrome
func formatModelPricing(slug string, p map[string]any) map[string]any {
	inpRaw, outRaw := pricingField(p, "prompt"), pricingField(p, "completion")
	cacheRaw := pricingField(p, "input_cache_read")
	formatRaw := func(raw any) string {
		if s, ok := raw.(string); ok && s == "" {
			return ""
		}
		return FormatPricePerMtok(rawPriceString(raw))
	}
	inp, out := formatRaw(inpRaw), formatRaw(outRaw)
	var cache any
	if pyTruthy(cacheRaw) {
		cache = FormatPricePerMtok(rawPriceString(cacheRaw))
	}
	entry := map[string]any{"input": inp, "output": out, "cache": cache, "free": inp == "free" && (out == "free" || out == "")}
	if slug == "nous" {
		if sale, ok := ComputeSaleDiscount(rawPriceString(inpRaw), rawPriceString(outRaw), p["original"]); ok {
			entry["discount_percent"] = sale.Percent
			if sale.WasPrompt != "" {
				entry["was_input"] = FormatPricePerMtok(sale.WasPrompt)
			}
			if sale.WasCompletion != "" {
				entry["was_output"] = FormatPricePerMtok(sale.WasCompletion)
			}
		}
	}
	return entry
}

// Local row of staged GGUF models, current by alias or by the managed server endpoint, nil when none staged
func localRuntimeRow(ctx context.Context, pc PickerConfigContext) map[string]any {
	staged := inventoryStagedModelsFn()
	if len(staged) == 0 {
		return nil
	}
	current := LlamacppAliases[strings.ToLower(strings.TrimSpace(pc.CurrentProvider))]
	if !current {
		baseURL, _, ok, err := inventoryStateEndpointFn(home.Dir(ctx))
		current = err == nil && ok &&
			strings.TrimRight(strings.TrimSpace(pc.CurrentBaseURL), "/") == strings.TrimRight(baseURL, "/")
	}
	return inventoryRow(LlamacppProviderID, "Local", current, map[string]any{"models": stringsToAny(staged),
		"total_models": len(staged), "source": "local-runtime", "authenticated": true, "auth_type": "local", "warning": nil})
}

// Warms picker pricing caches in the background, one live worker per profile and endpoint scope
func prewarmPricingAsync(ctx context.Context, rows []map[string]any, currentProvider, currentBaseURL string) <-chan struct{} {
	slugs := map[[2]string]bool{}
	for _, row := range rows {
		if !pyTruthy(row["slug"]) {
			continue
		}
		slug := strings.ToLower(pyOrStr(row["slug"]))
		baseURL := ""
		if strings.HasPrefix(slug, "custom:") {
			baseURL = pyOrStr(row["api_url"])
		}
		slugs[[2]string{slug, baseURL}] = true
	}
	var scope [][2]string
	for pair := range slugs {
		scope = append(scope, [2]string{pair[0], inventoryPricingScopeFn(ctx, pair[0], pair[1], currentProvider, currentBaseURL)})
	}
	sort.Slice(scope, func(i, j int) bool {
		if scope[i][0] != scope[j][0] {
			return scope[i][0] < scope[j][0]
		}
		return scope[i][1] < scope[j][1]
	})
	var key strings.Builder
	key.WriteString(home.Key(home.Dir(ctx)))
	for _, pair := range scope {
		key.WriteString("\x00" + pair[0] + "\x01" + pair[1])
	}
	prewarmKey := key.String()
	inventoryPricingPrewarmMu.Lock()
	defer inventoryPricingPrewarmMu.Unlock()
	if current, ok := inventoryPricingPrewarmRuns[prewarmKey]; ok {
		select {
		case <-current:
		default:
			return current
		}
	}
	workerRows := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		cp := make(map[string]any, len(row))
		for k, v := range row {
			cp[k] = v
		}
		cp["models"] = stringsToAny(inventoryModels(row))
		workerRows = append(workerRows, cp)
	}
	done := make(chan struct{})
	inventoryPricingPrewarmRuns[prewarmKey] = done
	workerCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			inventoryPricingPrewarmMu.Lock()
			if inventoryPricingPrewarmRuns[prewarmKey] == done {
				delete(inventoryPricingPrewarmRuns, prewarmKey)
			}
			inventoryPricingPrewarmMu.Unlock()
			close(done)
		}()
		applyPricing(workerCtx, workerRows, false, false)
	}()
	return done
}

// Virtual Mixture of Agents row listing the presets, nil unless the raw config enables one
func moaProviderRow(ctx context.Context, currentProvider string) map[string]any {
	if ctx == nil {
		ctx = context.Background()
	}
	if !rawConfigHasEnabledMoAPreset(ctx) {
		return nil
	}
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil
	}
	models := moaPresetOrder(anyMap(normalizeMoAConfig(tree["moa"])["presets"]))
	if len(models) == 0 {
		return nil
	}
	return inventoryRow("moa", "Mixture of Agents", strings.ToLower(currentProvider) == "moa", map[string]any{
		"models": stringsToAny(models), "total_models": len(models), "source": "virtual", "authenticated": true,
		"auth_type": "virtual",
		"warning":   "Aggregator is the acting model billed for the run; references only advise once per user turn by default.",
	})
}
