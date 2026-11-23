package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Flow of each provider id with a bespoke icebath model step, swappable per entry like the table
var providerModelFlows = map[string]func(r *ModelFlowRun) error{
	"openrouter":    modelFlowOpenrouter,
	"moa":           modelFlowMoA,
	"ai-gateway":    modelFlowAIGateway,
	"openai-codex":  modelFlowOpenAICodex,
	"xai-oauth":     modelFlowXAIOAuth,
	"qwen-oauth":    modelFlowQwenOAuth,
	"minimax-oauth": modelFlowMinimaxOAuth,
	"custom":        modelFlowCustom,
	"llamacpp":      modelFlowLocal,
	"anthropic":     modelFlowAnthropic,
	"kimi-coding":   modelFlowKimi,
	"stepfun":       modelFlowStepfun,
	"bedrock":       modelFlowBedrock,
	"vertex":        modelFlowVertex,
	"azure-foundry": modelFlowAzureFoundry,
	"copilot":       modelFlowCopilot,
	"copilot-acp":   modelFlowCopilotACP,
}

// Model command seams, swappable so tests script the picker and the flows like the swaps
var (
	pickProviderFn                = pickProvider
	pickerPromptProviderChoiceFn  = promptProviderChoice
	clearStaleOpenAIBaseURLFn     = clearStaleOpenAIBaseURL
	modelFlowAPIKeyProviderFn     = modelFlowAPIKeyProvider
	modelFlowNamedCustomFn        = modelFlowNamedCustom
	selectProviderAndModelFn      = SelectProviderAndModel
	pickerResolveAutoProviderFn   = func(env PoolEnv) (string, error) { return resolveProvider(env, "auto") }
	clearProviderModelsCacheFn    = ClearAllProviderModelsCache
	modelCommandLabelsFn          = catalogProviderLabelsCopy
	modelCommandProfileFlowFn     = profileModelSetupFlow
	modelCommandPluginFlowCheckFn = isProfilePluginFlowProvider
	offerReasoningAfterPickFn     = offerReasoningAfterPick
)

// Saved custom providers keyed by slug in config order, the value of a repeated slug replaced in place
type customProviderMap struct {
	keys  []string
	byKey map[string]map[string]any
}

func newCustomProviderMap() *customProviderMap {
	return &customProviderMap{byKey: map[string]map[string]any{}}
}

func (m *customProviderMap) set(key string, info map[string]any) {
	if _, ok := m.byKey[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.byKey[key] = info
}

func (m *customProviderMap) get(key string) (map[string]any, bool) {
	info, ok := m.byKey[key]
	return info, ok
}

// Copy of the provider label table with the custom endpoint label
func catalogProviderLabelsCopy() map[string]string {
	labels := map[string]string{}
	for _, p := range CanonicalProviders() {
		labels[p.Slug] = CatalogProviderLabel(p.Slug)
	}
	labels["custom"] = CatalogProviderLabel("custom")
	return labels
}

// Label of a slug from the table, the slug itself when the table lacks it
func labelOr(labels map[string]string, slug string) string {
	if label, ok := labels[slug]; ok {
		return label
	}
	return slug
}

// Saved custom providers keyed by slug, each with the raw env reference template of its key preserved
func namedCustomProviderMap(r *ModelFlowRun, cfg map[string]any) (*customProviderMap, error) {
	rawCfg, err := config.ReadRawConfig(r.ctx(), r.home())
	if err != nil {
		return nil, err
	}
	rawAPIKeyRefs := map[string]string{}
	if providers, ok := rawCfg["providers"].(map[string]any); ok {
		for _, k := range config.MappingKeys(providers) {
			entry, isMap := providers[k].(map[string]any)
			if !isMap {
				continue
			}
			if template := pystr.Strip(pyOrStr(dictGet(entry, "api_key", ""))); strings.Contains(template, "${") {
				rawAPIKeyRefs[pystr.Lower(storedProviderID(providers, k))] = template
			}
		}
	}
	out := newCustomProviderMap()
	for _, entry := range ProviderEntries(cfg) {
		if entry == nil {
			continue
		}
		rawName, err := pyOrEmptyStr(entry["name"], "providers name")
		if err != nil {
			return nil, err
		}
		rawBase, err := pyOrEmptyStr(entry["base_url"], "providers base_url")
		if err != nil {
			return nil, err
		}
		name, baseURL := pystr.Strip(rawName), pystr.Strip(rawBase)
		if name == "" || baseURL == "" {
			continue
		}
		rawKey, err := pyOrEmptyStr(entry["provider_key"], "providers provider_key")
		if err != nil {
			return nil, err
		}
		providerKey := pystr.Strip(rawKey)
		model := dictGet(entry, "model", "")
		keyEnv := entry["key_env"]
		out.set(CustomProviderSlug(name, providerKey), map[string]any{
			"name":              name,
			"base_url":          baseURL,
			"api_key":           dictGet(entry, "api_key", ""),
			"key_env":           keyEnv,
			"key_cmd":           dictGet(entry, "key_cmd", ""),
			"model":             model,
			"models":            dictGet(entry, "models", map[string]any{}),
			"models_discovered": dictGet(entry, "models_discovered", false),
			"extra_headers":     dictGet(entry, "extra_headers", map[string]any{}),
			"discover_models":   dictGet(entry, "discover_models", true),
			"api_mode":          dictGet(entry, "api_mode", ""),
			"provider_key":      providerKey,
			"api_key_ref":       rawAPIKeyRefs[pystr.Lower(providerKey)],
		})
	}
	return out, nil
}

// providerPickerRow is one icebath model picker row, a group row carrying the member slugs of its sub picker
type providerPickerRow struct {
	Key, Label string
	Members    []string
}

// Picker rows with canonical providers folded into groups, saved custom providers and the trailing actions
func buildProviderPickerRows(ctx context.Context, cfg map[string]any, active string, providerLabels map[string]string, customMap *customProviderMap) ([]providerPickerRow, int, error) {
	canonical := CanonicalProviders()
	canonicalDescs := map[string]string{}
	for _, p := range canonical {
		canonicalDescs[p.Slug] = p.TUIDesc
	}
	listed := listedCanonicalProviders(ctx)
	excluded := map[string]bool{}
	var modelCatalog map[string]any
	if raw := cfg["model_catalog"]; pyTruthy(raw) {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("model_catalog in config is %s, expected a mapping", errchain.Kind(raw))
		}
		modelCatalog = m
	}
	if raw := modelCatalog["excluded_providers"]; pyTruthy(raw) {
		items, err := pyIterValues(raw, "model_catalog.excluded_providers")
		if err != nil {
			return nil, 0, err
		}
		for _, p := range items {
			if pyTruthy(p) {
				excluded[pystr.Lower(pystr.Strip(pyStr(p)))] = true
			}
		}
	}
	var visible []string
	if len(excluded) > 0 {
		namesFor := map[string]map[string]bool{}
		for _, p := range canonical {
			namesFor[p.Slug] = map[string]bool{pystr.Lower(p.Slug): true}
		}
		for alias, canon := range CatalogProviderAliases() {
			names, ok := namesFor[canon]
			if !ok {
				names = map[string]bool{pystr.Lower(canon): true}
				namesFor[canon] = names
			}
			names[pystr.Lower(alias)] = true
		}
		for _, p := range listed {
			names, ok := namesFor[p.Slug]
			if !ok {
				names = map[string]bool{pystr.Lower(p.Slug): true}
			}
			hidden := false
			for n := range names {
				if excluded[n] {
					hidden = true
					break
				}
			}
			if !hidden {
				visible = append(visible, p.Slug)
			}
		}
	} else {
		for _, p := range listed {
			visible = append(visible, p.Slug)
		}
	}
	activeGroup := ""
	if active != "" {
		activeGroup = ProviderGroupForSlug(active)
	}
	var ordered []providerPickerRow
	defaultIdx := 0
	add := func(key, label string, members []string, isActive bool) {
		if isActive {
			label += "  ← currently active"
			defaultIdx = len(ordered)
		}
		ordered = append(ordered, providerPickerRow{Key: key, Label: label, Members: members})
	}
	for _, row := range GroupProviders(visible) {
		if row.Kind == "group" {
			label := row.Label + " ▸"
			if row.Description != "" {
				label = fmt.Sprintf("%s ▸ (%s)", row.Label, row.Description)
			}
			add("group:"+row.GroupID, label, row.Members, activeGroup != "" && row.GroupID == activeGroup)
			continue
		}
		slug := row.Slug
		label, ok := canonicalDescs[slug]
		if !ok {
			label = labelOr(providerLabels, slug)
		}
		add(slug, label, nil, active != "" && slug == active)
	}
	for _, key := range customMap.keys {
		info := customMap.byKey[key]
		modelHint := ""
		if saved := dictGet(info, "model", ""); pyTruthy(saved) {
			modelHint = " — " + pyStr(saved)
		}
		add(key, fmt.Sprintf("%s (%s)%s", pyStr(info["name"]), shortURL(pyStr(info["base_url"])), modelHint), nil, active != "" && key == active)
	}
	llamacppExcluded := false
	for _, alias := range llamacppAliases {
		if excluded[alias] {
			llamacppExcluded = true
		}
	}
	if !llamacppExcluded && localModelsAvailableFn() {
		add("llamacpp", "Local models (run open models on this machine — no account or API key)", nil, active == "llamacpp")
	}
	ordered = append(ordered, providerPickerRow{Key: "custom", Label: "Custom endpoint (enter URL manually)"})
	if providers, ok := cfg["providers"].(map[string]any); ok && len(providers) > 0 {
		ordered = append(ordered, providerPickerRow{Key: "remove-custom", Label: "Remove a saved custom provider"})
	}
	ordered = append(ordered,
		providerPickerRow{Key: "reasoning", Label: "Reasoning effort for the current model..."},
		providerPickerRow{Key: "aux-config", Label: "Configure auxiliary models..."},
		providerPickerRow{Key: "cancel", Label: "Leave unchanged"})
	return ordered, defaultIdx, nil
}

// Base URL lowercased with whitespace and trailing slashes removed
func normBaseURL(u any) string {
	return pystr.Lower(pystr.RstripChars(pystr.Strip(pyOrStr(u)), "/"))
}

// Provider slug in effect for the picker default row, empty when no provider is set up yet
func resolveActiveProvider(r *ModelFlowRun, cfg map[string]any, modelCfg any, effectiveProvider string, customMap *customProviderMap) (string, error) {
	active := ""
	if m, ok := modelCfg.(map[string]any); ok && effectiveProvider == "custom" {
		if currentBase := normBaseURL(dictGet(m, "base_url", "")); currentBase != "" {
			for _, k := range customMap.keys {
				if normBaseURL(dictGet(customMap.byKey[k], "base_url", "")) == currentBase {
					active = k
					break
				}
			}
		}
	}
	if active == "" && effectiveProvider != "auto" {
		userProviders, _ := cfg["providers"].(map[string]any)
		compat := ProviderEntries(cfg)
		customProviders := make([]any, len(compat))
		for i, e := range compat {
			customProviders[i] = e
		}
		if activeDef := ResolveProviderFull(r.ctx(), effectiveProvider, userProviders, customProviders); activeDef != nil {
			active = activeDef.ID
			if activeDef.Source == "user-config" {
				requested := pystr.Lower(pystr.Strip(active))
				for _, k := range customMap.keys {
					info := customMap.byKey[k]
					if CustomProviderAliases(pyOrStr(dictGet(info, "name", "")), pyOrStr(dictGet(info, "provider_key", "")))[requested] {
						active = k
						break
					}
				}
			}
		} else {
			hint := ProviderInstallHint(r.ctx(), effectiveProvider)
			if hint == "" {
				hint = "Check 'icebath model' for available providers, or run 'icebath doctor' to diagnose config issues."
			}
			fmt.Fprintf(r.Out, "Warning: Unknown provider '%s'. %s Falling back to auto provider detection.\n", effectiveProvider, hint)
		}
	}
	if active == "" {
		resolved, err := pickerResolveAutoProviderFn(r.Env)
		var authErr *AuthError
		switch {
		case errors.As(err, &authErr):
			if authErr.Code == "no_provider_configured" {
				fmt.Fprintln(r.Out, "No provider is set up yet — pick one below. (LM Studio and other local servers work without an API key.)")
			} else if effectiveProvider == "auto" {
				fmt.Fprintf(r.Out, "Warning: %s Falling back to auto provider detection.\n", FormatAuthError(authErr))
			}
			active = ""
		case err != nil:
			return "", err
		default:
			active = resolved
		}
	}
	if active == "openrouter" {
		baseURL, _, err := config.GetEnvValue(r.ctx(), r.home(), "OPENAI_BASE_URL")
		if err != nil {
			return "", err
		}
		if baseURL != "" {
			active = "custom"
		}
	}
	return active, nil
}

// Provider picker with the group member sub picker, the concrete slug or false on cancel
func pickProvider(r *ModelFlowRun, cfg map[string]any, active string, providerLabels map[string]string, customMap *customProviderMap) (string, bool, error) {
	ordered, defaultIdx, err := buildProviderPickerRows(r.ctx(), cfg, active, providerLabels, customMap)
	if err != nil {
		return "", false, err
	}
	labels := make([]string, len(ordered))
	for i, row := range ordered {
		labels[i] = row.Label
	}
	idx, ok := pickerPromptProviderChoiceFn(r.Out, labels, defaultIdx, "Select provider:")
	if !ok || ordered[idx].Key == "cancel" {
		return "", false, nil
	}
	selected := ordered[idx]
	if len(selected.Members) == 0 {
		return selected.Key, true, nil
	}
	memberLabels := make([]string, len(selected.Members))
	for i, m := range selected.Members {
		memberLabels[i] = labelOr(providerLabels, m)
	}
	memberDefault := 0
	for i, m := range selected.Members {
		if m == active {
			memberDefault = i
			break
		}
	}
	groupLabel, _, _ := strings.Cut(selected.Label, " ▸")
	memberIdx, ok := pickerPromptProviderChoiceFn(r.Out, memberLabels, memberDefault, fmt.Sprintf("Select %s provider:", groupLabel))
	if !ok {
		return "", false, nil
	}
	return selected.Members[memberIdx], true, nil
}

// Pool environment of the context's profile home for one picker run
func modelCommandEnv(ctx context.Context) PoolEnv {
	return modelsPoolEnv(ctx)
}

// SelectProviderAndModel runs the provider picker, the provider's credential and model flow, and the effort step
func SelectProviderAndModel(ctx context.Context, ns map[string]any) error {
	env := modelCommandEnv(ctx)
	r := &ModelFlowRun{Ctx: ctx, Env: env, Out: SetupOutput(), Args: ns}
	tree, err := r.loadConfig()
	if err != nil {
		return err
	}
	cfg := map[string]any(tree)
	r.Config = cfg
	modelCfg := cfg["model"]
	var currentModelRaw any = modelCfg
	var configProvider any
	if m, ok := modelCfg.(map[string]any); ok {
		currentModelRaw = dictGet(m, "default", "")
		configProvider = m["provider"]
	}
	currentModel := "(not set)"
	if pyTruthy(currentModelRaw) {
		currentModel = pyStr(currentModelRaw)
	}
	r.CurrentModel = currentModel
	effectiveProvider := "auto"
	if pyTruthy(configProvider) {
		effectiveProvider = pyStr(configProvider)
	} else if v := os.Getenv("ICEBATH_INFERENCE_PROVIDER"); v != "" {
		effectiveProvider = v
	}
	customMap, err := namedCustomProviderMap(r, cfg)
	if err != nil {
		return err
	}
	active, err := resolveActiveProvider(r, cfg, modelCfg, effectiveProvider, customMap)
	if err != nil {
		return err
	}
	providerLabels := modelCommandLabelsFn()
	activeLabel := "none"
	if info, ok := customMap.get(active); ok && active != "" {
		activeLabel = pyStr(info["name"])
	} else if active != "" {
		activeLabel = labelOr(providerLabels, active)
	}
	fmt.Fprintln(r.Out)
	fmt.Fprintf(r.Out, "  Current model:    %s\n", currentModel)
	fmt.Fprintf(r.Out, "  Active provider:  %s\n", activeLabel)
	fmt.Fprintln(r.Out)
	selected, ok, err := pickProviderFn(r, cfg, active, providerLabels, customMap)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	switch selected {
	case "aux-config":
		return auxConfigMenu(r)
	case "reasoning":
		return promptMainReasoningEffortFn(r, currentModel, active)
	}
	savesBefore := modelChoiceSaveCount()
	providerGone := false
	err = func() error {
		if flow := providerModelFlows[selected]; flow != nil {
			return flow(r)
		}
		if profileFlow := modelCommandProfileFlowFn(ctx, selected); profileFlow != nil {
			var args any
			if ns != nil {
				args = ns
			}
			_, err := profileFlow(ctx, cfg, currentModel, args)
			return err
		}
		if modelCommandPluginFlowCheckFn(selected) {
			return modelFlowPluginProvider(r, selected)
		}
		if _, saved := customMap.get(selected); strings.HasPrefix(selected, "custom:") || saved {
			fresh, err := r.loadConfig()
			if err != nil {
				return err
			}
			freshMap, err := namedCustomProviderMap(r, fresh)
			if err != nil {
				return err
			}
			providerInfo, ok := freshMap.get(selected)
			if !ok {
				fmt.Fprintln(r.Out, "Warning: the selected saved custom provider is no longer available. "+
					"It may have been removed from config.yaml. No change.")
				providerGone = true
				return nil
			}
			return modelFlowNamedCustomFn(r, providerInfo)
		}
		if selected == "remove-custom" {
			return removeCustomProvider(r)
		}
		if genericAPIKeyProviders[selected] || isProfileAPIKeyProvider(ctx, selected) {
			return modelFlowAPIKeyProviderFn(r, selected)
		}
		return nil
	}()
	if err != nil || providerGone {
		return err
	}
	if err := offerReasoningAfterPickFn(r, currentModel, savesBefore); err != nil {
		return err
	}
	switch {
	case selected == "custom", selected == "cancel", selected == "remove-custom", strings.HasPrefix(selected, "custom:"):
		return nil
	}
	return clearStaleOpenAIBaseURLFn(r)
}

// IsPromptInterrupt reports the KeyboardInterrupt or EOFError a picker prompt raised
func IsPromptInterrupt(err error) bool {
	return errors.Is(err, errFlowCancelled) || errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF)
}

// CmdModel is icebath model after the terminal check, the cache refresh then the picker under setup navigation
func CmdModel(ctx context.Context, ns map[string]any, w io.Writer) (code int, err error) {
	restore := WithSetupOutput(w)
	defer restore()
	defer RecoverSetupExit(&code)
	if pyTruthy(ns["refresh"]) {
		func() {
			defer func() { _ = recover() }()
			clearProviderModelsCacheFn(ctx)
			fmt.Fprintln(w, "  Cleared model picker cache.")
		}()
	}
	return 0, RunSetupActionWithNavigation(ctx, "Model & Provider", func(ctx context.Context) error {
		return selectProviderAndModelFn(ctx, ns)
	}, flowNoChange)
}
