package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Model validation used by the switch pipeline, replaced in tests
var switchValidateModelFn = ValidateRequestedModel

// Models dev capabilities and metadata of the switched model, replaced in tests
var (
	switchCapabilitiesFn = ModelsDevCapabilities
	switchModelInfoFn    = ModelsDevModelInfo
	switchUsageNoticeFn  = LargeModelNoticeFor
)

// Serializes model selection writes to a config file
var modelSelectionConfigMu sync.Mutex

// Normalizes and validates the model for the target, accepting config declared models the remote catalog lacks
func validateSwitch(ctx context.Context, st *modelSwitch) *ModelSwitchResult {
	st.NewModel = resolveNamedCustomModelID(st.NewModel, st.TargetProvider, st.CustomProviders)
	st.NewModel = NormalizeModelForProvider(st.NewModel, st.TargetProvider)
	failOnModel := func(message string) *ModelSwitchResult {
		res := st.failOnTarget(message)
		res.NewModel = st.NewModel
		return res
	}
	if IsKnownNonChatModel(st.NewModel) {
		return failOnModel("`" + st.NewModel + "` is a generation model and cannot be used for chat. " +
			"Pick a chat model, or use image generation for image models.")
	}
	var headers map[string]string
	switch {
	case strings.ToLower(pyStripSpace(st.TargetProvider)) == "ollama" && st.SuppressOllamaHeaders:
		headers = map[string]string{}
	case strings.ToLower(pyStripSpace(st.TargetProvider)) == "ollama" && len(st.ValidationHeaders) > 0:
		headers = st.ValidationHeaders
	case strings.ToLower(pyStripSpace(st.TargetProvider)) == "ollama":
		headers = switchOllamaHeadersFn(ctx)
	case len(st.ValidationHeaders) > 0:
		headers = st.ValidationHeaders
	default:
		if entry, ok := st.UserProviders[st.TargetProvider]; ok {
			headers = extraHeadersFromConfig(entry)
		}
	}
	validateAs := st.TargetProvider
	if !strings.HasPrefix(strings.ToLower(validateAs), "custom") {
		pdef := ResolveProviderFull(ctx, validateAs, st.UserProviders, st.CustomProviders)
		if pdef != nil && pdef.Source == "user-config" && pdef.BaseURL != "" {
			validateAs = "custom:" + validateAs
		}
	}
	verdict := guardedSwitchValidation(ctx, st.NewModel, validateAs, ValidateModelOptions{
		APIKey: st.APIKey, BaseURL: st.BaseURL, APIMode: st.APIMode, Headers: headers})
	if !verdict.Accepted {
		if !configDeclaresModel(st.NewModel, st.TargetProvider, st.BaseURL, st.UserProviders, st.CustomProviders) {
			return failOnModel(verdict.Message)
		}
		verdict = ModelVerdict{Accepted: true, Persist: true, Recognized: false, Message: verdict.Message}
	}
	st.Validation = verdict
	return nil
}

// Validation verdict where a raising validator becomes a rejected verdict naming the failure
func guardedSwitchValidation(ctx context.Context, model, provider string, opts ValidateModelOptions) (verdict ModelVerdict) {
	defer func() {
		if rec := recover(); rec != nil {
			verdict = ModelVerdict{Message: fmt.Sprintf("Could not validate `%s`: %v", model, rec)}
		}
	}()
	return switchValidateModelFn(ctx, model, provider, opts)
}

// Providers whose api mode follows the final model, keyed on the provider family or id
var providerAPIModeOverrides = map[string]func(ctx context.Context, provider, model, apiKey string) string{
	"opencode-zen": opencodeAPIMode, "opencode-go": opencodeAPIMode, "opencode": opencodeAPIMode,
	"nous": nousSwitchAPIMode, "nous-portal": nousSwitchAPIMode, "nousresearch": nousSwitchAPIMode,
}

// OpenCode wire derived from the target model so a previous model mode never carries across switches
func opencodeAPIMode(_ context.Context, provider, model, _ string) string {
	return string(OpenCodeModelMode(provider, model))
}

// Nous Portal wire derived from the final model and the configured anthropic wire
func nousSwitchAPIMode(ctx context.Context, _, model, _ string) string {
	return string(NousMode(model, modelsResolver(ctx).nousAnthropicWire()))
}

// ModelDerivedAPIMode is the api mode re-derived from the final model, false when the endpoint fixes the wire
func ModelDerivedAPIMode(ctx context.Context, provider, model, apiKey string) (string, bool) {
	key := strings.ToLower(pyStripSpace(provider))
	lookup := OpenCodeFamily(key)
	if lookup == "" {
		lookup = key
	}
	if override, ok := providerAPIModeOverrides[lookup]; ok {
		return override(ctx, key, model, apiKey), true
	}
	if key == "" {
		return "", false
	}
	profile := perModelProfile(key)
	if profile == nil {
		return "", false
	}
	mode := modelAPIMode(profile, model, apiKey)
	return string(mode), mode != ""
}

// Shapes the final api mode and base URL and gathers capabilities, metadata, warnings and request overrides
func buildSwitchResult(ctx context.Context, st *modelSwitch) ModelSwitchResult {
	if derived, ok := ModelDerivedAPIMode(ctx, st.TargetProvider, st.NewModel, st.APIKey); ok {
		st.APIMode = derived
	}
	if st.APIMode == "" {
		st.APIMode = switchDetermineAPIMode(ctx, st.TargetProvider, st.BaseURL, st.NewModel)
	}
	if OpenCodeFamily(st.TargetProvider) != "" {
		st.BaseURL = NormalizeOpenCodeBaseURL(st.TargetProvider, APIMode(st.APIMode), st.BaseURL)
	}
	res := ModelSwitchResult{
		Success: true, NewModel: st.NewModel, TargetProvider: st.TargetProvider, ProviderChanged: st.providerChanged(),
		APIKey: st.APIKey, BaseURL: st.BaseURL, APIMode: st.APIMode, ProviderLabel: st.ProviderLabel,
		ResolvedViaAlias: st.ResolvedAlias, IsGlobal: st.IsGlobal, AliasEndpointApplied: st.AliasEndpointApplied,
	}
	if caps, ok := switchCapabilitiesFn(ctx, st.TargetProvider, st.NewModel, true); ok {
		res.Capabilities = &caps
	}
	isCodex := strings.ToLower(pyStripSpace(st.TargetProvider)) == "openai-codex"
	res.RuntimeCapabilities = map[string]bool{
		"native_compaction": resolveNativeCompactionCapability(st.NewModel, st.BaseURL, st.TargetProvider, isCodex)}
	if info, ok := switchModelInfoFn(ctx, st.TargetProvider, st.NewModel, true); ok {
		res.ModelInfo = &info
	}
	var warnings []string
	for _, w := range []string{st.Validation.Message, checkHermesModelWarning(st.NewModel)} {
		if w != "" {
			warnings = append(warnings, w)
		}
	}
	res.WarningMessage = strings.Join(warnings, " | ")
	res.UsageNotice = switchUsageNoticeFn(ctx, st.NewModel, st.TargetProvider, true, "")
	res.RequestOverrides = map[string]any{}
	r := modelsResolver(ctx)
	if cp := r.getNamedCustomProvider(st.TargetProvider, r.cfg, true); cp != nil {
		for k, v := range customProviderRequestOverrides(cp) {
			res.RequestOverrides[k] = v
		}
	}
	return res
}

// SwitchModelOptions is the current session route and the typed request of one model switch
type SwitchModelOptions struct {
	RawInput         string
	CurrentProvider  string
	CurrentBaseURL   string
	CurrentAPIKey    string
	IsGlobal         bool
	ExplicitProvider string
	UserProviders    map[string]any
	CustomProviders  []any
}

// SwitchModel routes, resolves credentials for and validates a model switch shared by the CLI and gateway
func SwitchModel(ctx context.Context, opts SwitchModelOptions) (ModelSwitchResult, error) {
	st := &modelSwitch{
		SwitchRoute: SwitchRoute{NewModel: pyStripSpace(opts.RawInput), TargetProvider: opts.CurrentProvider, ValidationHeaders: map[string]string{}},
		RawInput:    opts.RawInput, CurrentProvider: opts.CurrentProvider, CurrentBaseURL: opts.CurrentBaseURL,
		CurrentAPIKey: opts.CurrentAPIKey, IsGlobal: opts.IsGlobal, ExplicitProvider: opts.ExplicitProvider,
		UserProviders: opts.UserProviders, CustomProviders: opts.CustomProviders,
	}
	route := routeFromModelInput
	if opts.ExplicitProvider != "" {
		route = routeExplicitProvider
	}
	for _, step := range []func(context.Context, *modelSwitch) (*ModelSwitchResult, error){
		route, resolveSwitchCredentials,
		func(ctx context.Context, st *modelSwitch) (*ModelSwitchResult, error) {
			return validateSwitch(ctx, st), nil
		},
	} {
		fail, err := step(ctx, st)
		if err != nil {
			return ModelSwitchResult{}, err
		}
		if fail != nil {
			return *fail, nil
		}
	}
	return buildSwitchResult(ctx, st), nil
}

// ModelSelectionConfigUpdates is the ordered model section change a persisted selection makes, a nil value clearing the key
func ModelSelectionConfigUpdates(result ModelSwitchResult, currentModelCfg any) []config.KeyValue {
	modelCfg, _ := currentModelCfg.(map[string]any)
	orNil := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	updates := []config.KeyValue{
		{Key: "default", Value: result.NewModel}, {Key: "provider", Value: result.TargetProvider},
		{Key: "base_url", Value: orNil(result.BaseURL)}, {Key: "api_mode", Value: orNil(result.APIMode)},
	}
	if _, pinned := modelCfg["context_length"]; pinned {
		if ShouldClearContextPin(pyStrOrEmpty(pyOr(modelCfg["default"], modelCfg["model"])), result.NewModel,
			pyStrOrEmpty(modelCfg["base_url"]), result.BaseURL, pyStrOrEmpty(modelCfg["provider"]), result.TargetProvider) {
			updates = append(updates, config.KeyValue{Key: "context_length"})
		}
	}
	target := strings.ToLower(pyStripSpace(result.TargetProvider))
	routeChanged := SelectionRouteChanged(modelCfg, result)
	var stale []string
	if !strings.HasPrefix(target, "custom") || routeChanged {
		stale = []string{"api_key", "api"}
	}
	if routeChanged {
		stale = append(stale, "key_env", "api_key_env")
	}
	for _, key := range stale {
		if _, held := modelCfg[key]; held {
			updates = append(updates, config.KeyValue{Key: key})
		}
	}
	return updates
}

// SelectionRouteChanged reports whether the provider or endpoint differs between a model section and the switch target
func SelectionRouteChanged(modelCfg map[string]any, result ModelSwitchResult) bool {
	if strings.ToLower(pyStripSpace(pyStrOrEmpty(pyOr(modelCfg["provider"], "")))) != strings.ToLower(pyStripSpace(result.TargetProvider)) {
		return true
	}
	return NormalizeRouteBaseURL(pyStrOrEmpty(modelCfg["base_url"])) != NormalizeRouteBaseURL(result.BaseURL)
}

// ApplyModelSelection returns the model section with the selection applied, cleared keys removed or masked with null
func ApplyModelSelection(modelCfg any, result ModelSwitchResult, maskCleared bool) map[string]any {
	return mergeSelectionUpdates(modelCfg, ModelSelectionConfigUpdates(result, modelCfg), maskCleared)
}

// Copy of the model section with updates applied, a nil update removing the key unless masking a held key
func mergeSelectionUpdates(modelCfg any, updates []config.KeyValue, maskCleared bool) map[string]any {
	out := map[string]any{}
	if m, ok := modelCfg.(map[string]any); ok {
		for k, v := range m {
			out[k] = v
		}
	}
	for _, u := range updates {
		if _, held := out[u.Key]; u.Value == nil && !(maskCleared && held) {
			delete(out, u.Key)
		} else {
			out[u.Key] = u.Value
		}
	}
	return out
}

// RemoteModelSelectionGuard refuses a model assignment changing any model key an upper config level locks
func RemoteModelSelectionGuard(ctx context.Context, before, after any) error {
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if fileTooling {
		return nil
	}
	b, _ := before.(map[string]any)
	a, _ := after.(map[string]any)
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		return err
	}
	h := home.Dir(ctx)
	keySet := map[string]bool{}
	for k := range b {
		keySet[k] = true
	}
	for k := range a {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		bv, bHeld := b[key]
		av, aHeld := a[key]
		if bHeld == aHeld && pyJSONEqual(bv, av) {
			continue
		}
		level, _, err := backend.Locked(ctx, h, "model."+key)
		if err != nil {
			return err
		}
		if level != "" {
			return config.NewConfigLockedError("model."+key, level, "")
		}
	}
	return nil
}

// Raw user config document at a path, empty when missing or not a mapping
func readUserConfigRaw(ctx context.Context, path string) (map[string]any, error) {
	doc, err := config.ReadConfigDoc(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	m, _ := doc.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// PersistModelSelection writes a successful switch into the config at path, the active profile config when empty
func PersistModelSelection(ctx context.Context, result ModelSwitchResult, configPath string) error {
	path := configPath
	if path == "" {
		path = home.ConfigPath(home.Dir(ctx))
	}
	raw, err := readUserConfigRaw(ctx, path)
	if err != nil {
		return err
	}
	plannedFrom := raw["model"]
	updates := ModelSelectionConfigUpdates(result, plannedFrom)
	fileTooling, err := config.SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if !fileTooling {
		masked := mergeSelectionUpdates(plannedFrom, updates, true)
		var changes config.Changes
		for _, u := range updates {
			if v, held := masked[u.Key]; held {
				changes.Set = append(changes.Set, config.KeyValue{Key: "model." + u.Key, Value: v})
			} else {
				changes.Unset = append(changes.Unset, "model."+u.Key)
			}
		}
		return config.WriteChanges(ctx, path, changes)
	}
	if err := persistSelectionLocked(ctx, path, result, plannedFrom, updates); err != nil {
		return err
	}
	_ = os.Chmod(path, 0o600)
	return nil
}

// Merges the selection into a fresh read of the config and replaces it, recomputing when the model section moved
func persistSelectionLocked(ctx context.Context, path string, result ModelSwitchResult, plannedFrom any, updates []config.KeyValue) error {
	modelSelectionConfigMu.Lock()
	defer modelSelectionConfigMu.Unlock()
	cfg, err := readUserConfigRaw(ctx, path)
	if err != nil {
		return err
	}
	modelCfg := cfg["model"]
	if !pyJSONEqual(modelCfg, plannedFrom) {
		updates = ModelSelectionConfigUpdates(result, modelCfg)
	}
	merged := mergeSelectionUpdates(modelCfg, updates, false)
	if m, ok := modelCfg.(map[string]any); ok {
		clear(m)
		for k, v := range merged {
			m[k] = v
		}
	} else {
		cfg["model"] = merged
	}
	return config.WriteConfigDocument(ctx, path, cfg, "")
}

// Normalized extra headers of a provider config entry, empty for a non mapping
func extraHeadersFromConfig(entry any) map[string]string {
	m, ok := entry.(map[string]any)
	if !ok {
		return map[string]string{}
	}
	headers := NormalizeExtraHeaders(m["extra_headers"])
	if headers == nil {
		headers = map[string]string{}
	}
	return headers
}
