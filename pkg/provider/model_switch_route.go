package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Configured default MoA preset name
func moaDefaultPreset(ctx context.Context) string {
	cfg := modelsConfig(ctx)
	name, _ := normalizeMoAConfig(pyOr(cfg["moa"], map[string]any{}))["default_preset"].(string)
	return name
}

// Runtime resolution used by the switch credential steps, replaced in tests
var switchResolveRuntimeFn = func(ctx context.Context, requested, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	return modelsResolver(ctx).ResolveRuntime(requested, explicitAPIKey, explicitBaseURL, nil, targetModel)
}

// Local endpoint model probe for a provider switch without a model, replaced in tests
var switchAutoDetectLocalModelFn = func(ctx context.Context, baseURL string) string {
	return modelsResolver(ctx).autoDetectLocalModel(baseURL)
}

// Static catalog provider detection used as the last routing rung, replaced in tests
var switchDetectProviderFn = DetectProviderForModel

// Configured Ollama headers and native catalog detection used by the switch, replaced in tests
var (
	switchOllamaHeadersFn       = getOllamaRequestHeaders
	switchOllamaNativeCatalogFn = shouldUseOllamaNativeCatalog
)

// modelSwitch is the mutable state threaded through the switch steps
type modelSwitch struct {
	SwitchRoute
	RawInput             string
	CurrentProvider      string
	CurrentBaseURL       string
	CurrentAPIKey        string
	IsGlobal             bool
	ExplicitProvider     string
	UserProviders        map[string]any
	CustomProviders      []any
	ResolvedAlias        string
	AliasEndpointApplied bool
	ProviderLabel        string
	Validation           ModelVerdict
}

// Failed switch result carrying the global flag
func (st *modelSwitch) fail(message string) *ModelSwitchResult {
	return &ModelSwitchResult{Success: false, IsGlobal: st.IsGlobal, ErrorMessage: message}
}

// Failed switch result carrying the settled target provider and label
func (st *modelSwitch) failOnTarget(message string) *ModelSwitchResult {
	res := st.fail(message)
	res.TargetProvider, res.ProviderLabel = st.TargetProvider, st.ProviderLabel
	return res
}

func (st *modelSwitch) providerChanged() bool { return st.TargetProvider != st.CurrentProvider }

// Fills key, base URL, mode and headers from runtime resolution, headers kept when none resolve
func (st *modelSwitch) resolveRuntime(ctx context.Context, requested, explicitAPIKey, explicitBaseURL string) error {
	rt, err := switchResolveRuntimeFn(ctx, requested, explicitAPIKey, explicitBaseURL, st.NewModel)
	if err != nil {
		return err
	}
	if rt == nil {
		rt = &Runtime{}
	}
	st.APIKey, st.BaseURL = rt.APIKey, rt.BaseURL
	st.APIMode = string(rt.APIMode)
	if len(rt.ExtraHeaders) > 0 {
		st.ValidationHeaders = rt.ExtraHeaders
	}
	return nil
}

// Wire protocol for a provider endpoint with the configured Nous anthropic wire
func switchDetermineAPIMode(ctx context.Context, provider, baseURL, model string) string {
	return string(determineModeCtx(ctx, provider, baseURL, model, modelsResolver(ctx).nousAnthropicWire()))
}

// Routes an explicit provider request, detecting a local model when none was typed, then resolves the alias there
func routeExplicitProvider(ctx context.Context, st *modelSwitch) (*ModelSwitchResult, error) {
	pdef := ResolveProviderFull(ctx, st.ExplicitProvider, st.UserProviders, st.CustomProviders)
	if pdef == nil && strings.ToLower(pyStripSpace(st.ExplicitProvider)) == "custom" {
		pdef = bareCustomProviderDef(st.CurrentBaseURL)
	}
	if pdef == nil {
		return st.fail(unknownProviderMessage(ctx, st.ExplicitProvider)), nil
	}
	st.TargetProvider, st.ProviderLabel = pdef.ID, pdef.Name
	if st.TargetProvider == "moa" && st.NewModel == "" {
		st.NewModel = moaDefaultPreset(ctx)
	}
	if aggErr := aggregatorAliasError(ctx, st.ExplicitProvider, st.TargetProvider, st.CurrentProvider, st.UserProviders, st.CustomProviders); aggErr != "" {
		return st.failOnTarget(aggErr), nil
	}
	if st.NewModel == "" {
		if pdef.BaseURL == "" {
			return st.failOnTarget(fmt.Sprintf("Provider '%s' has no base URL configured. "+
				"Specify a model: /model <model-name> --provider %s", pdef.Name, st.ExplicitProvider)), nil
		}
		st.NewModel = switchAutoDetectLocalModelFn(ctx, pdef.BaseURL)
		if st.NewModel == "" {
			return st.failOnTarget(fmt.Sprintf("No model detected on %s (%s). "+
				"Specify the model explicitly: /model <model-name> --provider %s", pdef.Name, pdef.BaseURL, st.ExplicitProvider)), nil
		}
	}
	match, err := ResolveAlias(ctx, st.NewModel, st.TargetProvider, st.UserProviders, st.CustomProviders)
	var amb *AmbiguousAliasError
	if errors.As(err, &amb) {
		res := st.fail(ambiguousAliasMessage(amb))
		res.TargetProvider = st.TargetProvider
		return res, nil
	}
	if err != nil {
		return nil, err
	}
	if match != nil {
		st.NewModel = match.Model
		if providerIdentity(ctx, match.Provider, st.UserProviders, st.CustomProviders) == providerIdentity(ctx, st.TargetProvider, st.UserProviders, st.CustomProviders) {
			st.ResolvedAlias = match.Alias
		}
	}
	return nil, nil
}

// Resolves a family alias absent from the current provider against the authenticated providers
func routeAliasFallback(ctx context.Context, st *modelSwitch, key string) (*ModelSwitchResult, error) {
	authed := GetAuthenticatedProviderSlugs(ctx, st.CurrentProvider, st.UserProviders, st.CustomProviders)
	fallback, err := resolveAliasFallback(ctx, st.RawInput, authed, st.UserProviders, st.CustomProviders)
	var amb *AmbiguousAliasError
	if errors.As(err, &amb) {
		return st.fail(ambiguousAliasMessage(amb)), nil
	}
	if err != nil {
		return nil, err
	}
	if fallback == nil {
		identity := ModelAliases[key]
		return st.fail(fmt.Sprintf("Alias '%s' maps to %s/%s but no matching model was found in any provider catalog. "+
			"Try specifying the full model name.", key, identity.Vendor, identity.Family)), nil
	}
	st.TargetProvider, st.NewModel, st.ResolvedAlias = fallback.Provider, fallback.Model, fallback.Alias
	slog.Debug(fmt.Sprintf("Alias '%s' resolved via fallback to %s on %s", st.ResolvedAlias, st.NewModel, st.TargetProvider))
	return nil, nil
}

// Turns vendor colon model into a vendor slash slug on aggregators or when the vendor names a known provider
func convertVendorColonSlug(ctx context.Context, st *modelSwitch) {
	raw := st.RawInput
	colon := strings.Index(raw, ":")
	curNorm := strings.ToLower(pyStripSpace(st.CurrentProvider))
	if colon <= 0 || strings.Contains(raw, "/") || strings.HasPrefix(curNorm, "custom") || curNorm == "ollama" {
		return
	}
	left := strings.ToLower(pyStripSpace(raw[:colon]))
	right := pyStripSpace(raw[colon+1:])
	if left == "" || right == "" {
		return
	}
	if !isAggregatorCtx(ctx, st.CurrentProvider) && !namesKnownProvider(ctx, left, st) {
		return
	}
	st.NewModel = left + "/" + right
	slog.Debug(fmt.Sprintf("Converted vendor:model '%s' to slug '%s'", raw, st.NewModel))
}

// Whether the name is a built in provider id or alias or a provider the user configured
func namesKnownProvider(ctx context.Context, name string, st *modelSwitch) bool {
	if ResolveProviderFull(ctx, name, st.UserProviders, st.CustomProviders) != nil {
		return true
	}
	return GetProviderDef(ctx, name, false) != nil
}

// Routes a model declared in provider config to its provider, true when routed
func routeConfiguredProvider(st *modelSwitch) (*ModelSwitchResult, bool) {
	matches := configuredProviderMatches(st.NewModel, st.UserProviders, st.CustomProviders)
	if len(matches) == 0 {
		return nil, false
	}
	if slug, ok := currentProviderMatch(st.CurrentProvider, st.UserProviders, matches); ok {
		st.NewModel = matches[configuredMatchIndex(matches, slug)].Model
		return nil, true
	}
	slugs := make([]string, 0, len(matches))
	for _, m := range matches {
		slugs = append(slugs, m.Slug)
	}
	sort.Strings(slugs)
	if len(slugs) > 1 {
		return st.fail(fmt.Sprintf("'%s' is declared by multiple configured providers (%s). Re-run with "+
			"--provider <slug> to choose which one to use.", st.NewModel, strings.Join(slugs, ", "))), false
	}
	st.TargetProvider = slugs[0]
	st.NewModel = matches[configuredMatchIndex(matches, st.TargetProvider)].Model
	slog.Debug(fmt.Sprintf("Configured-provider detection routed '%s' to %s", st.NewModel, st.TargetProvider))
	if _, ok := st.UserProviders[st.TargetProvider]; ok {
		st.ExplicitProvider = st.TargetProvider
	}
	return nil, true
}

// Routes typed model text through MoA presets, aliases, aggregator catalogs, config declarations and detection
func routeFromModelInput(ctx context.Context, st *modelSwitch) (*ModelSwitchResult, error) {
	raw, current := st.RawInput, st.CurrentProvider
	cfg := modelsConfig(ctx)
	if moaMatch := ExactMoAPresetName(normalizeMoAConfig(pyOr(cfg["moa"], map[string]any{})), raw); moaMatch != "" {
		st.TargetProvider, st.NewModel, st.ResolvedAlias = "moa", moaMatch, ""
	} else {
		match, err := ResolveAlias(ctx, raw, current, st.UserProviders, st.CustomProviders)
		var amb *AmbiguousAliasError
		if errors.As(err, &amb) {
			return st.fail(ambiguousAliasMessage(amb)), nil
		}
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(pyStripSpace(raw))
		_, isFamily := ModelAliases[key]
		switch {
		case match != nil:
			st.TargetProvider, st.NewModel, st.ResolvedAlias = match.Provider, match.Model, match.Alias
			slog.Debug(fmt.Sprintf("Alias '%s' resolved to %s on %s", st.ResolvedAlias, st.NewModel, st.TargetProvider))
		case isFamily:
			if fail, err := routeAliasFallback(ctx, st, key); fail != nil || err != nil {
				return fail, err
			}
		default:
			convertVendorColonSlug(ctx, st)
		}
	}
	resolvedInCurrentCatalog := false
	if isAggregatorCtx(ctx, st.TargetProvider) && st.ResolvedAlias == "" {
		if catalog := listProviderModelsFn(ctx, st.TargetProvider); len(catalog) > 0 {
			if matched, ok := aggregatorCatalogMatch(st.NewModel, catalog); ok {
				st.NewModel, resolvedInCurrentCatalog = matched, true
			}
		}
	}
	if st.ResolvedAlias != "" || resolvedInCurrentCatalog || st.TargetProvider != current {
		return nil, nil
	}
	if current == "nous" && RouteIsWelcomeHost(st.CurrentBaseURL) && st.NewModel != GuestModel {
		return st.fail(fmt.Sprintf("%s needs a Nous account or an API key. "+
			"Use /login to sign in, or /model to pick another provider.", st.NewModel)), nil
	}
	fail, configRouted := routeConfiguredProvider(st)
	if fail != nil {
		return fail, nil
	}
	host := baseURLHostname(st.CurrentBaseURL)
	isCustom := current == "custom" || current == "local" || strings.HasPrefix(current, "custom:") || host == "localhost" || host == "127.0.0.1"
	if !configRouted && !isCustom {
		if provider, model, ok := switchDetectProviderFn(ctx, st.NewModel, current); ok {
			st.TargetProvider, st.NewModel = provider, model
		}
	}
	return nil, nil
}

// Display label of the switch target, a named custom provider showing its own name
func switchProviderLabel(ctx context.Context, st *modelSwitch) string {
	label := GetProviderLabel(ctx, st.TargetProvider)
	if st.TargetProvider == "custom" && st.CurrentBaseURL != "" {
		label = "Custom endpoint"
	}
	if strings.HasPrefix(st.TargetProvider, "custom:") {
		if pdef := ResolveProviderFull(ctx, st.TargetProvider, st.UserProviders, st.CustomProviders); pdef != nil {
			label = pdef.Name
		}
	}
	return label
}

// Credentials for a changed or explicit provider from its providers block, the session endpoint or runtime resolution
func credsForSwitchedProvider(ctx context.Context, st *modelSwitch) *ModelSwitchResult {
	var userPdef *ProviderDef
	explicitNorm := strings.ToLower(pyStripSpace(st.ExplicitProvider))
	if st.ExplicitProvider != "" && len(st.UserProviders) > 0 {
		if userPdef = resolveUserProvider(explicitNorm, st.UserProviders); userPdef == nil {
			userPdef = resolveUserProvider(st.TargetProvider, st.UserProviders)
		}
	}
	switch {
	case userPdef != nil && userPdef.BaseURL != "":
		ucfg, _ := pyOr(st.UserProviders[explicitNorm], st.UserProviders[st.TargetProvider], map[string]any{}).(map[string]any)
		ukey := entryConfiguredKey(ucfg, func(name string) string {
			value, _ := scopedKeyEnv(ctx, name)
			return value
		})
		st.ValidationHeaders = extraHeadersFromConfig(ucfg)
		if err := st.resolveRuntime(ctx, st.TargetProvider, ukey, userPdef.BaseURL); err != nil {
			st.APIKey, st.BaseURL, st.APIMode = ukey, userPdef.BaseURL, ""
		} else {
			if st.APIKey == "" {
				st.APIKey = ukey
			}
			if st.BaseURL == "" {
				st.BaseURL = userPdef.BaseURL
			}
		}
	case st.TargetProvider == "custom" && st.CurrentBaseURL != "":
		key, url := st.CurrentAPIKey, st.CurrentBaseURL
		if st.CurrentProvider != "custom" {
			_ = st.resolveRuntime(ctx, "custom", "", "")
			if st.BaseURL != "" && !fellBackToOpenRouterDefault(ctx, st) {
				key, url = st.APIKey, st.BaseURL
			}
		}
		st.APIKey, st.BaseURL = key, url
		st.APIMode = switchDetermineAPIMode(ctx, st.TargetProvider, st.BaseURL, "")
	default:
		aliasURL := ""
		if st.ResolvedAlias != "" {
			if da, ok := DirectAliases.Get(st.ResolvedAlias); ok && modelsResolver(ctx).resolvesToCustom(st.TargetProvider) {
				aliasURL = da.BaseURL
			}
		}
		if err := st.resolveRuntime(ctx, st.TargetProvider, "", aliasURL); err != nil {
			if IsLlamaCppAlias(st.TargetProvider) {
				return st.failOnTarget(err.Error())
			}
			return st.failOnTarget(fmt.Sprintf("%s is not connected: no API key or login was found for it. Add one with "+
				"`icebath auth add %s`, or pick a connected provider in /model.\n  Details: %s", st.ProviderLabel, st.TargetProvider, err))
		}
	}
	return nil
}

// Credentials when staying on the current provider, a native Ollama session keeping its own endpoint
func credsForCurrentProvider(ctx context.Context, st *modelSwitch) {
	keepOllama := false
	ollamaHeaders := map[string]string{}
	if st.CurrentProvider == "custom" && st.CurrentBaseURL != "" {
		ollamaHeaders = switchOllamaHeadersFn(ctx)
		if _, configuredBase := ollamaConfiguredBase(ctx); configuredBase == "" || !sameOllamaNativeRoot(st.CurrentBaseURL, configuredBase) {
			ollamaHeaders = map[string]string{}
			st.SuppressOllamaHeaders = true
		}
		keepOllama = switchOllamaNativeCatalogFn(ctx, st.CurrentProvider, st.CurrentBaseURL, ollamaHeaders)
	}
	if keepOllama {
		st.APIKey = st.CurrentAPIKey
		if st.APIKey == "" {
			st.APIKey = "no-key-required"
		}
		st.BaseURL = st.CurrentBaseURL
		st.APIMode = switchDetermineAPIMode(ctx, st.CurrentProvider, st.BaseURL, "")
		st.ValidationHeaders = ollamaHeaders
		return
	}
	_ = st.resolveRuntime(ctx, st.CurrentProvider, "", "")
	if (st.CurrentProvider == "custom" || st.CurrentProvider == "local") && st.CurrentBaseURL != "" &&
		(st.BaseURL == "" || fellBackToOpenRouterDefault(ctx, st)) {
		st.BaseURL, st.APIKey = st.CurrentBaseURL, st.CurrentAPIKey
		st.APIMode = switchDetermineAPIMode(ctx, st.CurrentProvider, st.BaseURL, "")
	}
}

// Whether bare custom resolution landed on the OpenRouter default host or its unconfigured mirror
func fellBackToOpenRouterDefault(ctx context.Context, st *modelSwitch) bool {
	mirror := openrouterMirrorBaseURL(ctx)
	if mirror != "" && strings.TrimRight(st.BaseURL, "/") == mirror && customEndpointSource(ctx) == "" {
		return true
	}
	return baseURLHostMatches(st.BaseURL, "openrouter.ai") && !baseURLHostMatches(st.CurrentBaseURL, "openrouter.ai")
}

// Configured bare custom endpoint from CUSTOM_BASE_URL or a trusted model base URL, empty when none or unreadable
func customEndpointSource(ctx context.Context) string {
	envURL, ok, err := GetSecret(ctx, "CUSTOM_BASE_URL")
	if err != nil {
		return ""
	}
	if envURL = pyStripSpace(envURL); ok && envURL != "" {
		return envURL
	}
	r := modelsResolver(ctx)
	modelCfg := r.modelConfig()
	base, _ := modelCfg["base_url"].(string)
	provider, _ := modelCfg["provider"].(string)
	if base = pyStripSpace(base); base != "" && r.configBaseURLTrustworthyForBareCustom(base, provider) {
		return base
	}
	return ""
}

// OPENROUTER_BASE_URL without trailing slashes, empty when unset or the scoped read fails
func openrouterMirrorBaseURL(ctx context.Context) string {
	value, ok, err := GetSecret(ctx, "OPENROUTER_BASE_URL")
	if err != nil || !ok {
		return ""
	}
	return strings.TrimRight(pyStripSpace(value), "/")
}

// Settles credentials, a direct alias endpoint and the wire protocol for the final provider and base URL
func resolveSwitchCredentials(ctx context.Context, st *modelSwitch) (*ModelSwitchResult, error) {
	st.ProviderLabel = switchProviderLabel(ctx, st)
	st.APIKey, st.BaseURL = st.CurrentAPIKey, st.CurrentBaseURL
	if st.providerChanged() || st.ExplicitProvider != "" {
		if fail := credsForSwitchedProvider(ctx, st); fail != nil {
			return fail, nil
		}
	} else {
		credsForCurrentProvider(ctx, st)
	}
	if st.ResolvedAlias != "" {
		ensureDirectAliases(ctx)
		if da, ok := DirectAliases.Get(st.ResolvedAlias); ok && da.BaseURL != "" {
			if err := applyDirectAliasEndpoint(ctx, &st.SwitchRoute, da); err != nil {
				return nil, err
			}
			st.AliasEndpointApplied = true
		}
	}
	mandated := string(HostMandatedMode(st.BaseURL))
	if isActualRouteCtx(ctx, st.TargetProvider, st.BaseURL) {
		mandated = string(ModeChatCompletions)
	}
	if mandated != "" && st.APIMode != string(modeCodexAppServer) {
		st.APIMode = mandated
	}
	if st.APIMode == "" {
		st.APIMode = switchDetermineAPIMode(ctx, st.TargetProvider, st.BaseURL, "")
	}
	return nil, nil
}
