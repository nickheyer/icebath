package provider

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Canonical auxiliary task slots in the deterministic order the dashboard lists them
var auxTaskSlotsBuiltin = []string{"vision", "compression", "skills_hub", "approval", "mcp", "title_generation", "review",
	"voice_chat", "triage_specifier", "kanban_decomposer", "profile_describer", "curator"}

// Bare selectors whose submitted URL defines a route of its own
var bareCustomSelectors = map[string]bool{"custom": true, "local": true}

// Model switch pipeline and print destination of the assignment chain, the attributes hermes tests patch
var (
	assignmentSwitchModel           = SwitchModel
	assignmentOut         io.Writer = os.Stdout
)

// PreparedMainAssignment is a validated main slot pick computed outside the config mutation lock
type PreparedMainAssignment struct {
	BaseURL string
	Result  ModelSwitchResult
}

// AuxReasoningEffort is an optional assignment field, the zero value omitted and a nil Value clearing the override
type AuxReasoningEffort struct {
	Present bool
	Value   *string
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

// Plugin auxiliary task lookup of hermes_cli.plugins, the module attribute hermes tests patch
var pluginAuxiliaryTasksFn = plugins.GetPluginAuxiliaryTasks

// Plugin registered auxiliary tasks of the home in ctx whose keys are not built in slots, empty when discovery fails
func pluginAuxTasks(ctx context.Context) (out []map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			slog.Debug("plugin auxiliary task lookup failed", "error", failure)
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

// Every auxiliary slot the dashboard reads or assigns, built ins first then plugin keys
func auxTaskSlots(ctx context.Context) []string {
	out := append([]string(nil), auxTaskSlotsBuiltin...)
	for _, entry := range pluginAuxTasks(ctx) {
		out = append(out, entry["key"].(string))
	}
	return out
}

// Main slot pair as persisted, declared providers first, then the aggregator guess and model normalization
func normalizeMainModelAssignment(ctx context.Context, provider, model string) (string, string) {
	provIn := pystr.Strip(provider)
	modelIn := pystr.Strip(model)
	canonical := modelsNormalizeProvider(provIn)
	cfg, err := configLoadFn(ctx)
	if err != nil || cfg == nil {
		cfg = map[string]any{}
	}
	userProviders, _ := cfg["providers"].(map[string]any)
	declared := resolveUserProvider(provIn, userProviders)
	if declared == nil {
		declared = resolveCustomProvider(provIn, compatibleCustomProvidersList(cfg))
	}
	if declared != nil {
		return declared.ID, modelIn
	}
	isCustomSlug := canonical == "custom" || strings.HasPrefix(canonical, "custom:")
	if !knownProviderNames()[canonical] && !isCustomSlug && strings.Contains(modelIn, "/") {
		curProvider := ""
		if curCfg, ok := cfg["model"].(map[string]any); ok {
			curProvider = pystr.Lower(pystr.Strip(pyOrStr(pyGetAny(curCfg, "provider", ""))))
		}
		if curProvider != "" && IsAggregatorProvider(modelsNormalizeProvider(curProvider)) {
			canonical = modelsNormalizeProvider(curProvider)
			provIn = curProvider
		} else if providerHasCredentialsFn(ctx, "openrouter") {
			canonical, provIn = "openrouter", "openrouter"
		}
	}
	if knownProviderNames()[canonical] && !strings.HasPrefix(canonical, "custom") {
		if normalized := NormalizeModelForProvider(modelIn, canonical); normalized != "" {
			modelIn = normalized
		}
	}
	return provIn, modelIn
}

// Provider alias collapsed onto its canonical id, an empty name meaning openrouter
func modelsNormalizeProvider(provider string) string {
	if provider == "" {
		provider = "openrouter"
	}
	normalized := pystr.Lower(pystr.Strip(provider))
	if target, ok := CatalogProviderAlias(normalized); ok {
		return target
	}
	return normalized
}

// Deduplicated custom provider view of a config as the list the resolvers take
func compatibleCustomProvidersList(cfg map[string]any) []any {
	rows := CompatibleCustomProviders(cfg)
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = row
	}
	return out
}

// Trailing slash and case insensitive URL equality
func sameEndpoint(a, b any) bool {
	return pystr.Lower(strings.TrimRight(pystr.Strip(pyOrStr(a)), "/")) == pystr.Lower(strings.TrimRight(pystr.Strip(pyOrStr(b)), "/"))
}

// A configured provider block naming a different host than the submitted endpoint
func entryPointsElsewhere(entry any, baseURL string) bool {
	m, ok := entry.(map[string]any)
	return ok && !sameEndpoint(m["base_url"], baseURL)
}

// A bare custom or local pick carrying its own endpoint URL
func isExplicitCustomEndpoint(provider, baseURL string) bool {
	return bareCustomSelectors[pystr.Lower(pystr.Strip(provider))] && pystr.Strip(baseURL) != ""
}

// Routes a main slot pick through the model switch seeded with the configured route, a rejection becoming a 400
func validatedMainModelSelection(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string, explicitEndpoint *bool) (ModelSwitchResult, error) {
	modelCfg, _ := cfg["model"].(map[string]any)
	isBareCustom := bareCustomSelectors[pystr.Lower(pystr.Strip(provider))]
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
			if v := userProviders[k]; !(bareCustomSelectors[pystr.Lower(pystr.Strip(k))] && entryPointsElsewhere(v, baseURL)) {
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
		opts.CustomProviders = compatibleCustomProvidersList(cfg)
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
		return ModelSwitchResult{}, httpException(400, detail)
	}
	if url := pystr.Strip(baseURL); isBareCustom && url != "" {
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
	if key := pystr.Strip(apiKey); key != "" {
		assignKey(out, "api_key", key)
		if err := DropModelKey(ctx, out, "api"); err != nil {
			return nil, err
		}
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

// Carries the provider's credential pointer onto the model block, never a secret the load expanded
func resolveAssignmentCredentials(ctx context.Context, modelCfg map[string]any, provider string, providerEntry any) error {
	var rawEntry map[string]any
	if raw, err := config.ReadRawConfig(ctx, home.Dir(ctx)); err == nil {
		_, rawEntry = FindProviderEntry(raw["providers"], provider)
	}
	if rawEntry == nil {
		rawEntry = map[string]any{}
	}
	if keyEnv := pystr.Strip(pyOrStr(rawEntry["key_env"])); keyEnv != "" {
		assignKey(modelCfg, "key_env", keyEnv)
		return DropModelKey(ctx, modelCfg, "api_key")
	}
	if entry, ok := providerEntry.(map[string]any); ok && pyTruthy(entry["api_key"]) {
		rawKey := pystr.Strip(pyOrStr(rawEntry["api_key"]))
		if strings.HasPrefix(rawKey, "${") && strings.HasSuffix(rawKey, "}") {
			assignKey(modelCfg, "api_key", rawKey)
		} else {
			assignKey(modelCfg, "api_key", entry["api_key"])
		}
	}
	return nil
}

// Routes unconfigured tools through the Nous Tool Gateway after a main switch to Nous, the sorted changed keys
func applyNousGatewayDefaults(ctx context.Context, cfg map[string]any) []string {
	var enabled []string
	for name := range tools.Default.PlatformTools(ctx, tools.ScopeKey(home.Dir(ctx)), config.Tree(cfg), "cli", tools.PlatformToolsOptions{}) {
		enabled = append(enabled, name)
	}
	changed := ApplyNousManagedDefaults(modelsPoolEnv(ctx), cfg, enabled, true)
	out := []string{}
	for key := range changed {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Whether base_url is already an endpoint of the inline model block, a providers entry or a custom_providers row
func endpointKnownToConfig(cfg map[string]any, baseURL string) bool {
	wanted := pystr.Lower(strings.TrimRight(pystr.Strip(baseURL), "/"))
	if wanted == "" {
		return false
	}
	matches := func(value any) bool {
		return pystr.Lower(strings.TrimRight(pystr.Strip(pyOrStr(value)), "/")) == wanted
	}
	if modelCfg, ok := cfg["model"].(map[string]any); ok && matches(modelCfg["base_url"]) {
		return true
	}
	for _, sectionKey := range []string{"providers", "custom_providers"} {
		var entries []any
		switch section := cfg[sectionKey].(type) {
		case []any:
			entries = section
		case map[string]any:
			for _, k := range config.MappingKeys(section) {
				entries = append(entries, section[k])
			}
		}
		for _, raw := range entries {
			if entry, ok := raw.(map[string]any); ok && matches(pyOr(entry["base_url"], entry["url"], entry["api"])) {
				return true
			}
		}
	}
	return false
}

// Registers a named custom_providers row for a genuinely new custom endpoint, never failing the saved assignment
func registerCustomEndpoint(ctx context.Context, baseURL, apiKey, model string) {
	run := &ModelFlowRun{Ctx: ctx, Env: modelsPoolEnv(ctx), Out: assignmentOut}
	if err := saveCustomProvider(run, baseURL, apiKey, model, 0, autoProviderName(baseURL), "", ""); err != nil {
		slog.Debug(fmt.Sprintf("custom_providers registration skipped: %v", err))
	}
}

// Aux slots pinned to a provider other than the new main one, local endpoints and main followers excluded
func staleAuxPins(ctx context.Context, cfg map[string]any, newProvider string) []map[string]any {
	stale := []map[string]any{}
	auxCfg, ok := pyGetAny(cfg, "auxiliary", map[string]any{}).(map[string]any)
	if !ok {
		return stale
	}
	for _, slot := range auxTaskSlots(ctx) {
		slotCfg, ok := auxCfg[slot].(map[string]any)
		if !ok {
			continue
		}
		slotProvider := pystr.Strip(pyOrStr(pyGetAny(slotCfg, "provider", "")))
		lower := pystr.Lower(slotProvider)
		if slotProvider == "" || lower == "auto" || lower == "main" || lower == newProvider {
			continue
		}
		if IsLocalEndpoint(pyOrStr(pyGetAny(slotCfg, "base_url", ""))) {
			continue
		}
		stale = append(stale, map[string]any{"task": slot, "provider": slotProvider, "model": pyOrStr(pyGetAny(slotCfg, "model", ""))})
	}
	return stale
}

// The providers block of one provider, nil when the config has none
func providerEntryOf(cfg map[string]any, provider string) any {
	if providers, ok := cfg["providers"].(map[string]any); ok {
		return providers[provider]
	}
	return nil
}

// Validation half of a main slot assignment returning the effective base URL and switch result, writing nothing
func prepareMainAssignment(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string) (*PreparedMainAssignment, error) {
	if provider == "" || model == "" {
		return nil, httpException(400, "provider and model required for main")
	}
	explicit := isExplicitCustomEndpoint(provider, baseURL)
	if explicit {
		provider, model = pystr.Strip(provider), pystr.Strip(model)
	} else {
		provider, model = normalizeMainModelAssignment(ctx, provider, model)
	}
	if entry, ok := providerEntryOf(cfg, provider).(map[string]any); baseURL == "" && ok && pyTruthy(entry["base_url"]) {
		baseURL = pystr.Strip(pyOrStr(entry["base_url"]))
	}
	result, err := validatedMainModelSelection(ctx, cfg, provider, model, baseURL, apiKey, &explicit)
	if err != nil {
		return nil, err
	}
	return &PreparedMainAssignment{BaseURL: baseURL, Result: result}, nil
}

// PrepareMainAssignment validates a main slot pick against the loaded config before the config lock is taken
func PrepareMainAssignment(ctx context.Context, cfg map[string]any, provider, model, baseURL, apiKey string) (*PreparedMainAssignment, error) {
	return prepareMainAssignment(ctx, cfg, provider, model, baseURL, apiKey)
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
	providerEntry := providerEntryOf(cfg, provider)
	providerLC := pystr.Lower(pystr.Strip(provider))
	endpointAlreadyKnown := bareCustomSelectors[providerLC] && baseURL != "" && endpointKnownToConfig(cfg, baseURL)
	modelCfg, err := applyMainModelAssignment(ctx, pyGetAny(cfg, "model", map[string]any{}), result, apiKey)
	if err != nil {
		return nil, err
	}
	if !(explicit && entryPointsElsewhere(providerEntry, baseURL)) {
		if err := resolveAssignmentCredentials(ctx, modelCfg, provider, providerEntry); err != nil {
			return nil, err
		}
	}
	assignKey(cfg, "model", modelCfg)
	gatewayTools := []string{}
	if providerLC == "nous" {
		gatewayTools = applyNousGatewayDefaults(ctx, cfg)
	}
	if err := configSaveFn(config.RefusingLockedOmissions(ctx, "model"), cfg); err != nil {
		return nil, err
	}
	if bareCustomSelectors[providerLC] && baseURL != "" && !endpointAlreadyKnown {
		registerCustomEndpoint(ctx, baseURL, apiKey, model)
	}
	ReconcileSetupRecord(modelsPoolEnv(ctx))
	out := map[string]any{"ok": true, "scope": "main", "provider": provider, "model": model,
		"base_url": pyGetAny(modelCfg, "base_url", ""), "gateway_tools": gatewayTools, "stale_aux": staleAuxPins(ctx, cfg, providerLC)}
	if result.UsageNotice != "" {
		out["notice"] = result.UsageNotice
	}
	return out, nil
}

// Canonical auxiliary reasoning effort for an assignment, nil clearing to inherit and a 400 for an unknown level
func normalizeAuxReasoningEffort(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	parsed := config.ParseReasoningEffort(*value)
	if parsed == nil {
		return nil, httpException(400, "reasoning_effort must be one of: none, "+strings.Join(config.ValidReasoningEfforts, ", "))
	}
	level := "none"
	if parsed["enabled"] != false {
		level = parsed["effort"].(string)
	}
	return &level, nil
}

// Reasoning effort the shipped defaults give an auxiliary slot, empty when none
func auxDefaultEffort(slot string) string {
	aux, _ := map[string]any(config.Defaults())["auxiliary"].(map[string]any)
	if block, ok := aux[slot].(map[string]any); ok {
		return pyOrStr(block["reasoning_effort"])
	}
	return ""
}

// Persists an auxiliary slot assignment or the reset of every slot, echoing an explicit reasoning effort
func applyAuxAssignmentSync(ctx context.Context, cfg map[string]any, provider, model, task, baseURL, apiKey string, reasoningEffort AuxReasoningEffort) (map[string]any, error) {
	aux, ok := cfg["auxiliary"].(map[string]any)
	if !ok {
		aux = map[string]any{}
	}
	slotOf := func(slot string) map[string]any {
		if slotCfg, ok := aux[slot].(map[string]any); ok {
			return slotCfg
		}
		return map[string]any{}
	}
	var effort *string
	if reasoningEffort.Present {
		var err error
		if effort, err = normalizeAuxReasoningEffort(reasoningEffort.Value); err != nil {
			return nil, err
		}
	}
	slots := auxTaskSlots(ctx)
	if task == "__reset__" {
		for _, slot := range slots {
			slotCfg := slotOf(slot)
			assignKey(slotCfg, "provider", "auto")
			assignKey(slotCfg, "model", "")
			delete(slotCfg, "reasoning_effort")
			delete(slotCfg, "base_url")
			clearModelEndpointCredentials(slotCfg, true, true, false)
			assignKey(aux, slot, slotCfg)
		}
		assignKey(cfg, "auxiliary", aux)
		if err := configSaveFn(ctx, cfg); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "scope": "auxiliary", "reset": true}, nil
	}
	if provider == "" {
		return nil, httpException(400, "provider required for auxiliary")
	}
	targets := []string{task}
	if task == "" {
		targets = slots
	}
	known := map[string]bool{}
	for _, slot := range slots {
		known[slot] = true
	}
	newProvider := pystr.Lower(pystr.Strip(provider))
	for _, slot := range targets {
		if !known[slot] {
			return nil, httpException(400, "unknown auxiliary task: "+slot)
		}
		slotCfg := slotOf(slot)
		prevProvider := pystr.Lower(pystr.Strip(pyOrStr(slotCfg["provider"])))
		assignKey(slotCfg, "provider", provider)
		assignKey(slotCfg, "model", model)
		if baseURL != "" {
			assignKey(slotCfg, "base_url", baseURL)
			if apiKey != "" {
				assignKey(slotCfg, "api_key", apiKey)
			}
		} else if newProvider != prevProvider && newProvider != "custom" {
			delete(slotCfg, "base_url")
			clearModelEndpointCredentials(slotCfg, true, true, false)
		}
		if reasoningEffort.Present && effort == nil {
			if auxDefaultEffort(slot) != "" {
				assignKey(slotCfg, "reasoning_effort", "")
			} else {
				delete(slotCfg, "reasoning_effort")
			}
		} else if reasoningEffort.Present {
			assignKey(slotCfg, "reasoning_effort", *effort)
		}
		assignKey(aux, slot, slotCfg)
	}
	assignKey(cfg, "auxiliary", aux)
	if err := configSaveFn(ctx, cfg); err != nil {
		return nil, err
	}
	result := map[string]any{"ok": true, "scope": "auxiliary", "tasks": append([]string(nil), targets...), "provider": provider, "model": model}
	if reasoningEffort.Present {
		if effort == nil {
			result["reasoning_effort"] = nil
		} else {
			result["reasoning_effort"] = *effort
		}
	}
	return result, nil
}

// ApplyModelAssignmentSync is the body of the model set route, prepared carrying a validation done outside the config lock
func ApplyModelAssignmentSync(ctx context.Context, scope, provider, model, task, baseURL, apiKey string, reasoningEffort AuxReasoningEffort, prepared *PreparedMainAssignment) (map[string]any, error) {
	cfg, err := configLoadFn(ctx)
	if err != nil {
		return nil, err
	}
	if scope == "main" {
		return applyMainAssignmentSync(ctx, cfg, provider, model, baseURL, apiKey, prepared)
	}
	return applyAuxAssignmentSync(ctx, cfg, provider, model, task, baseURL, apiKey, reasoningEffort)
}

// Provider serving a model typed into the flat config field, empty when no switch is warranted
func inferProviderOnModelChange(ctx context.Context, modelVal, prevProvider string) (string, string) {
	name := pystr.Strip(modelVal)
	if name == "" {
		return "", name
	}
	if provider, model, ok := DetectProviderForModel(ctx, name, prevProvider); ok {
		return provider, model
	}
	if strings.Contains(name, "/") && !IsAggregatorProvider(modelsNormalizeProvider(prevProvider)) && providerHasCredentialsFn(ctx, "openrouter") {
		return "openrouter", name
	}
	return "", name
}

// InferProviderOnModelChange names the provider a model typed into the flat config field belongs to
func InferProviderOnModelChange(ctx context.Context, modelVal, prevProvider string) (string, string) {
	return inferProviderOnModelChange(ctx, modelVal, prevProvider)
}
