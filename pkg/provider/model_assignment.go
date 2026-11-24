package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Built in auxiliary task slots in canonical order
var auxTaskSlotsBuiltin = []string{"vision", "compression", "approval", "mcp", "title_generation", "review",
	"voice_chat", "triage_specifier", "kanban_decomposer", "profile_describer", "curator", "monitor"}

// Bare selectors whose submitted URL defines a route of its own
var bareCustomSelectors = map[string]bool{"custom": true, "local": true}

// Model switch pipeline and print destination of the assignment chain, the attributes tests patch
var (
	assignmentSwitchModel           = SwitchModel
	assignmentOut         io.Writer = os.Stdout
)

// PreparedMainAssignment is a validated main slot pick computed outside the config mutation lock
type PreparedMainAssignment struct {
	BaseURL string
	Result  ModelSwitchResult
}

// Sets key on m, a key m lacked moving to the end of its order like a Python dict insert
func assignKey(m map[string]any, key string, value any) {
	if _, held := m[key]; !held {
		m[key] = value
		config.AppendMappingKey(m, key)
		return
	}
	m[key] = value
}

// Plugin auxiliary task lookup of, the module attribute tests patch
var pluginAuxiliaryTasksFn = plugins.GetPluginAuxiliaryTasks

// Plugin registered auxiliary tasks of the home in ctx whose keys are not built in slots, empty when discovery fails
func pluginAuxTasks(ctx context.Context) (out []map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			pkgLog().Debug("plugin auxiliary task lookup failed", "error", failure)
			out = []map[string]any{}
		}
	}()
	builtin := map[string]bool{}
	for _, slot := range auxTaskSlotsBuiltin {
		builtin[slot] = true
	}
	for _, entry := range pluginAuxiliaryTasksFn(ctx) {
		key, _ := entry["key"].(string)
		if key == "" || builtin[key] {
			continue
		}
		copied := make(map[string]any, len(entry))
		for k, v := range entry {
			copied[k] = v
		}
		out = append(out, copied)
	}
	return out
}

// Every auxiliary slot, built ins first then plugin keys
func auxTaskSlots(ctx context.Context) []string {
	out := append([]string(nil), auxTaskSlotsBuiltin...)
	for _, entry := range pluginAuxTasks(ctx) {
		out = append(out, entry["key"].(string))
	}
	return out
}

// Main slot pair as persisted, declared providers first, then the aggregator guess and model normalization
func normalizeMainModelAssignment(ctx context.Context, provider, model string) (string, string) {
	provIn := strings.TrimSpace(provider)
	modelIn := strings.TrimSpace(model)
	canonical := modelsNormalizeProvider(provIn)
	cfg, err := configLoadFn(ctx)
	if err != nil || cfg == nil {
		cfg = map[string]any{}
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	declared := resolveUserProvider(provIn, userProviders)
	if declared == nil {
		declared = resolveCustomProvider(provIn, ProviderEntryList(cfg))
	}
	if declared != nil {
		return declared.ID, modelIn
	}
	isCustomSlug := canonical == "custom" || strings.HasPrefix(canonical, "custom:")
	if !knownProviderNames()[canonical] && !isCustomSlug && strings.Contains(modelIn, "/") {
		curProvider := ""
		if curCfg, ok := cfg["model"].(map[string]any); ok {
			curProvider = strings.ToLower(strings.TrimSpace(pyOrStr(pyGetAny(curCfg, "provider", ""))))
		}
		if curProvider != "" && IsAggregatorProvider(modelsNormalizeProvider(curProvider)) {
			canonical = modelsNormalizeProvider(curProvider)
			provIn = curProvider
		} else if providerHasCredentialsFn(ctx, "openrouter") {
			canonical, provIn = "openrouter", "openrouter"
		}
	}
	if knownProviderNames()[canonical] && !strings.HasPrefix(canonical, "custom") {
		if normalized := NormalizeModelForProviderCtx(ctx, modelIn, canonical); normalized != "" {
			modelIn = normalized
		}
	}
	return provIn, modelIn
}

// Is, aliases collapsed and an empty name meaning openrouter
func ModelsNormalizeProvider(provider string) string { return modelsNormalizeProvider(provider) }

// Provider alias collapsed onto its canonical id, an empty name meaning openrouter
func modelsNormalizeProvider(provider string) string {
	if provider == "" {
		provider = "openrouter"
	}
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if target, ok := CatalogProviderAlias(normalized); ok {
		return target
	}
	return normalized
}

// ProviderEntryList is the deduplicated providers entries of a config document a caller holds as a list of any, the shape the route options take
func ProviderEntryList(cfg map[string]any) []any {
	rows := ProviderEntriesOf(config.AsMap(cfg["providers"]))
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Trailing slash and case insensitive URL equality
func sameEndpoint(a, b any) bool {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(pyOrStr(a)), "/")) == strings.ToLower(strings.TrimRight(strings.TrimSpace(pyOrStr(b)), "/"))
}

// A configured provider block naming a different host than the submitted endpoint
func entryPointsElsewhere(entry map[string]any, baseURL string) bool {
	return entry != nil && !sameEndpoint(entry["base_url"], baseURL)
}

// A bare custom or local pick carrying its own endpoint URL
func isExplicitCustomEndpoint(provider, baseURL string) bool {
	return bareCustomSelectors[strings.ToLower(strings.TrimSpace(provider))] && strings.TrimSpace(baseURL) != ""
}

// Routes a main slot pick through the model switch seeded with the configured route, a refused pick returning ModelAssignmentRejected
func validatedMainModelSelection(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string, explicitEndpoint *bool) (ModelSwitchResult, error) {
	modelCfg, _ := cfg["model"].(map[string]any)
	isBareCustom := bareCustomSelectors[strings.ToLower(strings.TrimSpace(provider))]
	explicit := isExplicitCustomEndpoint(provider, baseURL)
	if explicitEndpoint != nil {
		explicit = *explicitEndpoint
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	if userProviders == nil {
		userProviders = map[string]any{}
	}
	if explicit {
		kept := map[string]any{}
		for _, k := range config.MappingKeys(userProviders) {
			v := userProviders[k]
			entry, _ := v.(map[string]any)
			if !(bareCustomSelectors[strings.ToLower(strings.TrimSpace(k))] && entryPointsElsewhere(entry, baseURL)) {
				assignKey(kept, k, v)
			}
		}
		userProviders = kept
	}
	opts := SwitchModelOptions{RawInput: model, ExplicitProvider: provider, IsGlobal: true,
		CurrentProvider: pyOrStr(pyGetAny(modelCfg, "provider", nil)), CurrentBaseURL: pyOrStr(pyGetAny(modelCfg, "base_url", nil)),
		UserProviders: userProviders, CustomProviders: []any{}}
	if explicit {
		opts.CurrentProvider = "custom"
	} else {
		opts.CustomProviders = ProviderEntryList(cfg)
	}
	if isBareCustom {
		opts.CurrentBaseURL, opts.CurrentAPIKey = baseURL, apiKey
	}
	result, err := assignmentSwitchModel(ctx, opts)
	if err != nil {
		return ModelSwitchResult{}, err
	}
	if !result.Success {
		detail := result.ErrorMessage
		if detail == "" {
			detail = "model switch rejected"
		}
		return ModelSwitchResult{}, &ModelAssignmentRejected{Msg: detail}
	}
	if url := strings.TrimSpace(baseURL); isBareCustom && url != "" {
		result.BaseURL = url
		result.APIMode = switchDetermineAPIMode(ctx, result.TargetProvider, url, "")
	}
	return result, nil
}

// Main slot selection applied to a model block through the canonical selection shape, an explicit key taking model.api_key
func applyMainModelAssignment(ctx context.Context, modelCfg any, result ModelSwitchResult, apiKey string) (map[string]any, error) {
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return nil, err
	}
	out := ApplyModelSelection(modelCfg, result, !fileTooling)
	if key := strings.TrimSpace(apiKey); key != "" {
		assignKey(out, "api_key", key)
	}
	if err := RemoteModelSelectionGuard(ctx, modelCfg, out); err != nil {
		return nil, err
	}
	return out, nil
}

// DropModelKey removes a model key the block holds, masking it with null under Remote Config
func DropModelKey(ctx context.Context, modelCfg map[string]any, key string) error {
	if _, held := modelCfg[key]; !held {
		return nil
	}
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if fileTooling {
		delete(modelCfg, key)
	} else {
		modelCfg[key] = nil
	}
	return nil
}

// Carries the credential of a provider's config block onto the model block the way config.yaml spells it, never a secret the load expanded
func resolveAssignmentCredentials(ctx context.Context, modelCfg map[string]any, entryKey string, entry map[string]any) error {
	if entry == nil {
		return nil
	}
	raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	// The key pointer names a variable and is safe from either document, the key itself is only ever copied as written
	_, spelled := FindProviderEntry(raw["providers"], entryKey)
	pointer := entry["key_env"]
	if spelled != nil {
		pointer = spelled["key_env"]
	}
	if keyEnv := strings.TrimSpace(pyOrStr(pointer)); keyEnv != "" {
		assignKey(modelCfg, "key_env", keyEnv)
		return DropModelKey(ctx, modelCfg, "api_key")
	}
	if key := spelled["api_key"]; pyTruthy(key) {
		assignKey(modelCfg, "api_key", key)
	}
	return nil
}

// Whether base_url is already an endpoint of the inline model block or a providers entry
func endpointKnownToConfig(cfg map[string]any, baseURL string) bool {
	wanted := strings.ToLower(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if wanted == "" {
		return false
	}
	matches := func(value any) bool {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(pyOrStr(value)), "/")) == wanted
	}
	if modelCfg, ok := cfg["model"].(map[string]any); ok && matches(modelCfg["base_url"]) {
		return true
	}
	providers, _ := cfg["providers"].(map[string]any)
	for _, raw := range providers {
		if entry, ok := raw.(map[string]any); ok && matches(entry["base_url"]) {
			return true
		}
	}
	return false
}

// Registers a providers entry for a genuinely new custom endpoint, never failing the saved assignment
func registerCustomEndpoint(ctx context.Context, baseURL, apiKey, model string) {
	run := &ModelFlowRun{Ctx: ctx, Env: modelsPoolEnv(ctx), Out: assignmentOut}
	if err := saveCustomProvider(run, baseURL, apiKey, model, 0, autoProviderName(baseURL), "", ""); err != nil {
		pkgLog().Debug(fmt.Sprintf("providers registration skipped: %v", err))
	}
}

// Aux slots pinned to a provider other than the new main one, local endpoints and main followers excluded
func staleAuxPins(ctx context.Context, cfg map[string]any, newProvider string) []map[string]any {
	stale := []map[string]any{}
	auxCfg, ok := pyGetAny(cfg, "auxiliary", map[string]any{}).(map[string]any)
	if !ok {
		return stale
	}
	// Every spelling of one providers entry is the same provider
	providers, _ := cfg["providers"].(map[string]any)
	identity := func(name string) string {
		return strings.ToLower(strings.TrimSpace(PersistedProviderID(ctx, strings.TrimSpace(name), providers)))
	}
	current := identity(newProvider)
	for _, slot := range auxTaskSlots(ctx) {
		slotCfg, ok := auxCfg[slot].(map[string]any)
		if !ok {
			continue
		}
		slotProvider := strings.TrimSpace(pyOrStr(pyGetAny(slotCfg, "provider", "")))
		lower := strings.ToLower(slotProvider)
		if slotProvider == "" || lower == "auto" || lower == "main" || identity(slotProvider) == current {
			continue
		}
		if IsLocalEndpoint(pyOrStr(pyGetAny(slotCfg, "base_url", ""))) {
			continue
		}
		stale = append(stale, map[string]any{"task": slot, "provider": slotProvider, "model": pyOrStr(pyGetAny(slotCfg, "model", ""))})
	}
	return stale
}

// Config key and block of the providers entry a provider's settings live in: the enabled entry the name routes to at runtime
// however the name spells it, else the block its own name keys, a nil block when the config holds neither
func providerEntryOf(ctx context.Context, cfg map[string]any, provider string) (string, map[string]any) {
	providers, _ := cfg["providers"].(map[string]any)
	if len(providers) == 0 {
		return "", nil
	}
	if key, routed := modelsResolver(ctx).providerEntryKey(provider, providers); routed {
		entry, _ := providers[key].(map[string]any)
		return key, entry
	}
	return FindProviderEntry(providers, provider)
}

// Validation half of a main slot assignment returning the effective base URL and switch result, writing nothing
func prepareMainAssignment(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string) (*PreparedMainAssignment, error) {
	if provider == "" || model == "" {
		return nil, httpException(400, "provider and model required for main")
	}
	explicit := isExplicitCustomEndpoint(provider, baseURL)
	if explicit {
		provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	} else {
		provider, model = normalizeMainModelAssignment(ctx, provider, model)
	}
	if baseURL == "" {
		if _, entry := providerEntryOf(ctx, cfg, provider); pyTruthy(entry["base_url"]) {
			baseURL = strings.TrimSpace(pyOrStr(entry["base_url"]))
		}
	}
	result, err := validatedMainModelSelection(ctx, cfg, provider, model, baseURL, apiKey, &explicit)
	if err != nil {
		var rejected *ModelAssignmentRejected
		if errors.As(err, &rejected) {
			return nil, httpException(400, rejected.Error())
		}
		return nil, err
	}
	return &PreparedMainAssignment{BaseURL: baseURL, Result: result}, nil
}

// Persists a main slot assignment, registers a new custom endpoint and reports stale aux pins
func applyMainAssignmentSync(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string, prepared *PreparedMainAssignment) (map[string]any, error) {
	explicit := isExplicitCustomEndpoint(provider, baseURL)
	if prepared == nil {
		var err error
		if prepared, err = prepareMainAssignment(ctx, cfg, provider, model, baseURL, apiKey); err != nil {
			return nil, err
		}
	}
	baseURL, result := prepared.BaseURL, prepared.Result
	provider, model = result.TargetProvider, result.NewModel
	entryKey, providerEntry := providerEntryOf(ctx, cfg, provider)
	providerLC := strings.ToLower(strings.TrimSpace(provider))
	endpointAlreadyKnown := bareCustomSelectors[providerLC] && baseURL != "" && endpointKnownToConfig(cfg, baseURL)
	modelCfg, err := applyMainModelAssignment(ctx, pyGetAny(cfg, "model", map[string]any{}), result, apiKey)
	if err != nil {
		return nil, err
	}
	if !(explicit && entryPointsElsewhere(providerEntry, baseURL)) {
		if err := resolveAssignmentCredentials(ctx, modelCfg, entryKey, providerEntry); err != nil {
			return nil, err
		}
	}
	assignKey(cfg, "model", modelCfg)
	if err := configSaveFn(config.RefusingLockedOmissions(ctx, "model"), cfg); err != nil {
		return nil, err
	}
	if bareCustomSelectors[providerLC] && baseURL != "" && !endpointAlreadyKnown {
		registerCustomEndpoint(ctx, baseURL, apiKey, model)
	}
	out := map[string]any{"ok": true, "scope": "main", "provider": provider, "model": model,
		"base_url": pyGetAny(modelCfg, "base_url", ""), "stale_aux": staleAuxPins(ctx, cfg, providerLC)}
	return out, nil
}

// Persists a main model pick, prepared carrying a validation done outside the config lock
func ApplyModelAssignmentSync(ctx context.Context, provider, model, baseURL, apiKey string, prepared *PreparedMainAssignment) (map[string]any, error) {
	cfg, err := configLoadFn(ctx)
	if err != nil {
		return nil, err
	}
	return applyMainAssignmentSync(ctx, cfg, provider, model, baseURL, apiKey, prepared)
}
