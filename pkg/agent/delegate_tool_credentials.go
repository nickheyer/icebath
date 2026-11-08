package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const runtimeProviderCustom = "custom"

var (
	delegationNativeSDKProviders = map[string]bool{"bedrock": true, "vertex": true, "google": true, "google-genai": true}
	delegationExplicitAPIModes   = map[string]bool{"chat_completions": true, "codex_responses": true, "anthropic_messages": true}
	delegationNousProviders      = map[string]bool{"nous": true, "nous-portal": true, "nousresearch": true}
)

func normalizedRuntimeURL(v any) string {
	s := ""
	if pyrepr.Truthy(v) {
		s = pyrepr.Str(v)
	}
	return strings.TrimRight(pystr.Strip(s), "/")
}

// Parent endpoint trust capabilities, inherited only when the child runs the parent's exact route
func inheritParentCapabilities(parent *AIAgent, overrideProvider, overrideBaseURL string) map[string]bool {
	if overrideProvider != "" || overrideBaseURL != "" || parent.capabilities == nil {
		return nil
	}
	out := make(map[string]bool, len(parent.capabilities))
	for k, v := range parent.capabilities {
		out[k] = v
	}
	return out
}

// Base url and key the parent is actually calling, both from the live client when it has one
func inheritParentEndpoint(parent *AIAgent, surfaceBaseURL string, surfaceAPIKey string) (string, string) {
	type candidate struct {
		url any
		key any
	}
	var candidates []candidate
	if parent.clientKwargs != nil {
		candidates = append(candidates, candidate{parent.clientKwargs["base_url"], parent.clientKwargs["api_key"]})
	}
	if parent.client != nil {
		var url, key any
		if c, ok := parent.client.(interface{ BaseURL() string }); ok {
			url = c.BaseURL()
		}
		if c, ok := parent.client.(interface{ APIKey() string }); ok {
			key = c.APIKey()
		}
		candidates = append(candidates, candidate{url, key})
	}
	for _, c := range candidates {
		url := normalizedRuntimeURL(c.url)
		if url != "" && (strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://")) {
			if key, ok := c.key.(string); ok && key != "" {
				return url, key
			}
			return url, surfaceAPIKey
		}
	}
	return surfaceBaseURL, surfaceAPIKey
}

// Loaded pool of a key when it holds credentials
func loadedPool(ctx context.Context, key string) *provider.Pool {
	pool := provider.LoadPool(agentPoolEnv(ctx), key)
	if pool != nil && pool.HasCredentials() {
		return pool
	}
	return nil
}

// Provider identity plus at least one entry serving the child's endpoint
func poolServesEndpoint(pool *provider.Pool, providerName, baseURL string) bool {
	if !provider.CredentialPoolMatchesProvider(pool.Provider(), providerName, baseURL) {
		return false
	}
	for _, entry := range pool.Entries() {
		if provider.CredentialPoolEntryServesEndpoint(entry, baseURL) {
			return true
		}
	}
	return false
}

// Credential pool of the child, the parent's for the same route, its provider's own, or none
func resolveChildCredentialPool(ctx context.Context, effectiveProvider string, parent *AIAgent, effectiveBaseURL, effectiveRequestedProvider string) (pool *provider.Pool) {
	var parentPool *provider.Pool
	parentProvider, parentBaseURL, parentRequested := "", "", ""
	if parent != nil {
		parentPool, parentProvider, parentBaseURL, parentRequested = parent.credentialPool, parent.provider, parent.baseURL, parent.requestedProvider
	}
	if effectiveProvider == "" {
		return parentPool
	}
	defer func() {
		if r := recover(); r != nil {
			if isUserInterruptPanic(r) {
				panic(r)
			}
			if effectiveProvider == "custom" {
				delegateLog().Debug(fmt.Sprintf("Could not resolve custom credential pool for child endpoint '%s': %v", effectiveBaseURL, r))
			} else {
				delegateLog().Debug(fmt.Sprintf("Could not load credential pool for child provider '%s': %v", effectiveProvider, r))
			}
			pool = nil
		}
	}()
	if effectiveProvider == "custom" {
		cfg := provider.LoadConfigSafe(home.Dir(ctx))
		childKey := provider.GetCustomProviderPoolKey(effectiveBaseURL, effectiveRequestedProvider, cfg)
		if childKey == "" {
			return nil
		}
		parentKey := provider.GetCustomProviderPoolKey(parentBaseURL, parentRequested, cfg)
		if parentPool != nil && parentProvider == "custom" && parentKey != "" && parentKey == childKey {
			return parentPool
		}
		var owner map[string]any
		if _, entry, ok := provider.CustomEntryForEndpoint(effectiveBaseURL, effectiveRequestedProvider, cfg); ok {
			owner = entry
		}
		return provider.EntryScopedPool(loadedPool(ctx, childKey), childKey, owner)
	}
	if effectiveProvider == parentProvider {
		if parentPool == nil {
			return nil
		}
		if effectiveBaseURL == "" || poolServesEndpoint(parentPool, effectiveProvider, effectiveBaseURL) {
			return parentPool
		}
		delegateLog().Debug(fmt.Sprintf("Parent %s pool has no entry for child endpoint %s; not sharing it", effectiveProvider, effectiveBaseURL))
	}
	loaded := loadedPool(ctx, effectiveProvider)
	if loaded != nil && effectiveBaseURL != "" && !poolServesEndpoint(loaded, effectiveProvider, effectiveBaseURL) {
		return nil
	}
	return loaded
}

// Explicit request overrides merged over runtime ones with extra_body merged one level, nil when both are empty
func mergeRequestOverrides(runtimeOverrides, explicitOverrides any) map[string]any {
	runtime, _ := runtimeOverrides.(map[string]any)
	explicitIn, _ := explicitOverrides.(map[string]any)
	if len(runtime) == 0 && len(explicitIn) == 0 {
		return nil
	}
	merged := map[string]any{}
	if len(runtime) > 0 {
		merged = pycopy.DeepCopy(runtime).(map[string]any)
	}
	explicit := map[string]any{}
	if len(explicitIn) > 0 {
		explicit = pycopy.DeepCopy(explicitIn).(map[string]any)
	}
	runtimeExtra := merged["extra_body"]
	explicitExtra, hasExplicitExtra := explicit["extra_body"]
	delete(explicit, "extra_body")
	for k, v := range explicit {
		merged[k] = v
	}
	re, rOK := runtimeExtra.(map[string]any)
	ee, eOK := explicitExtra.(map[string]any)
	if rOK && eOK {
		for k, v := range ee {
			re[k] = v
		}
		merged["extra_body"] = re
	} else if hasExplicitExtra && explicitExtra != nil {
		merged["extra_body"] = explicitExtra
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

type delegationValues struct {
	model, providerName, baseURL, apiKey, apiMode string
}

func overridesValue(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// Direct endpoint branch, provider and api mode from url heuristics with explicit api mode winning
func directEndpointCredentials(ctx context.Context, v delegationValues, explicitRequestOverrides map[string]any) map[string]any {
	baseLower := strings.ToLower(v.baseURL)
	host, _ := pyurl.BaseURLHostname(v.baseURL)
	providerName := "custom"
	apiMode := string(provider.DetectAPIModeForURL(v.baseURL))
	if apiMode == "" {
		apiMode = "chat_completions"
	}
	switch {
	case host == "chatgpt.com" && strings.Contains(baseLower, "/backend-api/codex"):
		providerName, apiMode = "openai-codex", "codex_responses"
	case host == "api.anthropic.com":
		providerName, apiMode = "anthropic", "anthropic_messages"
	case strings.Contains(baseLower, "api.kimi.com/coding"):
		apiMode = "anthropic_messages"
	}
	if delegationExplicitAPIModes[v.apiMode] || (v.apiMode != "" && provider.RegisteredAPIModes()[v.apiMode]) {
		apiMode = v.apiMode
	}
	var requestOverrides map[string]any
	if v.providerName != "" {
		runtime, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), v.providerName, v.model, "", "")
		if err != nil {
			delegateLog().Debug(fmt.Sprintf("delegation.base_url: runtime resolution for provider '%s' failed; proceeding without request_overrides: %v",
				v.providerName, err))
		} else if len(runtime.RequestOverrides) > 0 {
			requestOverrides = map[string]any{}
			for k, val := range runtime.RequestOverrides {
				requestOverrides[k] = val
			}
		}
	}
	return tools.CredentialBundle(noneIfEmpty(v.model), providerName, v.baseURL, noneIfEmpty(v.apiKey), apiMode,
		overridesValue(mergeRequestOverrides(requestOverrides, explicitRequestOverrides)), nil)
}

// Runtime provider branch, the full bundle through the runtime provider system
func runtimeProviderCredentials(ctx context.Context, v delegationValues, explicitRequestOverrides map[string]any) (map[string]any, error) {
	configured := v.providerName
	runtime, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), configured, v.model, "", "")
	if err != nil {
		return nil, &tools.DelegationValueError{Message: fmt.Sprintf("Cannot resolve delegation provider '%s': %v. "+
			"Check that the provider is configured (API key set, valid provider name), "+
			"or set delegation.base_url/delegation.api_key for a direct endpoint. "+
			"Available providers: openrouter, nous, zai, kimi-coding, minimax.", configured, err)}
	}
	if runtime.APIKey == "" {
		return nil, &tools.DelegationValueError{Message: fmt.Sprintf("Delegation provider '%s' resolved but has no API key. "+
			"Set the appropriate environment variable or run 'icebath auth'.", configured)}
	}
	pinned := runtime.Command
	if err := tools.RequirePinnedCommand(pinned, fmt.Sprintf("Delegation provider '%s' is pinned to the "+
		"'%s' command, which was not found on PATH. "+
		"Install it or choose a different delegation provider.", configured, pinned)); err != nil {
		return nil, err
	}
	if runtime.BaseURL == "" && pinned == "" && !delegationNativeSDKProviders[strings.ToLower(pystr.Strip(configured))] {
		return nil, &tools.DelegationValueError{Message: fmt.Sprintf("Delegation provider '%s' resolved without a base_url. "+
			"Refusing to build a subagent with an incomplete credential bundle — check the provider's "+
			"configuration / auth, or set delegation.base_url for a direct endpoint.", configured)}
	}
	model := v.model
	if model == "" {
		model = runtime.Model
	}
	providerName := runtime.Provider
	if runtime.Provider == runtimeProviderCustom {
		providerName = configured
	}
	overrides := mergeRequestOverrides(runtime.RequestOverrides, explicitRequestOverrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	args := []any{}
	for _, a := range runtime.Args {
		args = append(args, a)
	}
	return tools.CredentialBundle(noneIfEmpty(model), noneIfEmpty(providerName), noneIfEmpty(runtime.BaseURL), runtime.APIKey,
		noneIfEmpty(string(runtime.APIMode)), overrides, map[string]any{"command": noneIfEmpty(pinned), "args": args}), nil
}

func stripOrNone(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Child credential bundle from the delegation config, a direct endpoint, a runtime provider or pure inherit
func resolveDelegationCredentials(ctx context.Context, cfg map[string]any, parent *AIAgent) (map[string]any, error) {
	v := delegationValues{
		model: stripOrNone(cfg["model"]), providerName: stripOrNone(cfg["provider"]),
		baseURL: stripOrNone(cfg["base_url"]), apiKey: stripOrNone(cfg["api_key"]),
		apiMode: pystr.Lower(stripOrNone(cfg["api_mode"])),
	}
	explicit, _ := cfg["request_overrides"].(map[string]any)
	isNative := delegationNativeSDKProviders[strings.ToLower(pystr.Strip(v.providerName))]
	if v.baseURL != "" && !isNative {
		return directEndpointCredentials(ctx, v, explicit), nil
	}
	if v.providerName == "" {
		var parentOverrides any
		if parent != nil && parent.requestOverrides != nil {
			parentOverrides = parent.requestOverrides
		}
		return tools.CredentialBundle(noneIfEmpty(v.model), nil, nil, nil, nil,
			overridesValue(mergeRequestOverrides(parentOverrides, explicit)), nil), nil
	}
	return runtimeProviderCredentials(ctx, v, explicit)
}

// Fallback chain of a child owned by the same config block as its route
func resolveChildFallbackChain(parent *AIAgent, routingCfg map[string]any, pinned bool) []map[string]any {
	declared, declaredSet := any(nil), false
	if routingCfg != nil {
		declared, declaredSet = routingCfg["fallback_providers"]
	}
	return provider.ScopedFallbackChain(parent.fallbackChain, declared, declaredSet, pinned, "delegation")
}

// Child reasoning, a resolved route value as is, else route effort, delegation effort, then the parent's
func childReasoning(parent *AIAgent, delegationCfg map[string]any, routeEffort any) map[string]any {
	switch r := routeEffort.(type) {
	case tools.ResolvedReasoning:
		return r.Config
	case *tools.ResolvedReasoning:
		if r != nil {
			return r.Config
		}
	}
	reasoning := parent.reasoningConfig
	effort := routeEffort
	if !(pyrepr.Truthy(effort) || effort == false) {
		effort = delegationCfg["reasoning_effort"]
	}
	if pyrepr.Truthy(effort) || effort == false {
		parsed := config.ParseReasoningEffort(effort)
		if parsed == nil {
			delegateLog().Warn(fmt.Sprintf("Unknown delegation.reasoning_effort '%s', inheriting parent level", pyrepr.Str(effort)))
		} else {
			reasoning = parsed
		}
	}
	return reasoning
}

// Inputs of one child runtime resolution, the override keywords of the child build
type childRuntimeSpec struct {
	model, overrideProvider, overrideBaseURL, overrideAPIKey string
	overrideAPIMode                                          *string
	overrideACPCommand                                       string
	overrideACPArgs                                          []string
	routingCfg                                               map[string]any
	overrideReasoningEffort                                  any
}

// Child credentials, transport and routing as constructor options, config override over parent inherit
func resolveChildRuntime(ctx context.Context, parent *AIAgent, delegationCfg map[string]any, parentAPIKey string, s childRuntimeSpec) (AIAgentOptions, error) {
	effectiveModel := s.model
	if effectiveModel == "" {
		effectiveModel = parent.model
	}
	var effectiveProvider, effectiveBaseURL string
	switch {
	case s.overrideProvider != "":
		effectiveProvider, effectiveBaseURL = s.overrideProvider, s.overrideBaseURL
	case s.overrideBaseURL != "":
		effectiveProvider, effectiveBaseURL = parent.provider, s.overrideBaseURL
	default:
		effectiveProvider = parent.provider
		effectiveBaseURL, parentAPIKey = inheritParentEndpoint(parent, parent.baseURL, parentAPIKey)
	}
	var effectiveAPIMode string
	switch {
	case s.overrideAPIMode != nil:
		effectiveAPIMode = *s.overrideAPIMode
	case delegationNousProviders[strings.ToLower(pystr.Strip(effectiveProvider))]:
		effectiveAPIMode = provider.NousAPIMode(ctx, effectiveModel)
	case effectiveProvider != parent.provider:
		effectiveAPIMode = ""
	default:
		effectiveAPIMode = parent.apiMode
	}
	if err := tools.RequirePinnedCommand(s.overrideACPCommand, fmt.Sprintf("Pinned delegation command '%s' was not "+
		"found on PATH. Install it or remove delegation.command from config.yaml.", s.overrideACPCommand)); err != nil {
		return AIAgentOptions{}, err
	}
	acpCommand := s.overrideACPCommand
	if acpCommand == "" {
		acpCommand = parent.acpCommand
	}
	acpArgs := []string{}
	if s.overrideACPArgs != nil {
		acpArgs = append(acpArgs, s.overrideACPArgs...)
	} else {
		acpArgs = append(acpArgs, parent.acpArgs...)
	}
	if s.overrideProvider != "" && s.overrideACPCommand == "" {
		acpCommand, acpArgs = "", []string{}
	}
	requested := effectiveProvider
	if s.overrideProvider == "" && s.overrideBaseURL == "" && s.overrideACPCommand == "" && parent.requestedProvider != "" {
		requested = parent.requestedProvider
	}
	routing := delegationCfg
	if s.routingCfg != nil {
		routing = s.routingCfg
	}
	apiKey := s.overrideAPIKey
	if apiKey == "" {
		apiKey = parentAPIKey
	}
	o := AIAgentOptions{
		BaseURL: effectiveBaseURL, APIKey: apiKey, Model: effectiveModel, Provider: effectiveProvider,
		RequestedProvider: requested, Capabilities: inheritParentCapabilities(parent, s.overrideProvider, s.overrideBaseURL),
		APIMode: effectiveAPIMode, ACPCommand: acpCommand, ACPArgs: acpArgs,
		ReasoningConfig:          childReasoning(parent, delegationCfg, s.overrideReasoningEffort),
		FallbackModel:            resolveChildFallbackChain(parent, routing, s.overrideProvider != "" || s.overrideBaseURL != "" || s.model != ""),
		OpenRouterMinCodingScore: parent.openrouterMinCodingScore,
		ProviderDataCollection:   "",
	}
	if s.overrideProvider == "" {
		o.ProvidersAllowed, o.ProvidersIgnored, o.ProvidersOrder = parent.providersAllowed, parent.providersIgnored, parent.providersOrder
		o.ProviderSort, o.ProviderRequireParameters = parent.providerSort, parent.providerRequireParameters
		o.ProviderDataCollection = parent.providerDataCollection
		if !pyrepr.Truthy(o.ProviderDataCollection) {
			o.ProviderDataCollection = ""
		}
	}
	o.MaxTokens = parent.maxTokens
	return o, nil
}
