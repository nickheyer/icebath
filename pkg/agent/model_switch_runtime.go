package agent

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var switchModelContextLength = provider.GetModelContextLength

// Every field the swap and client rebuild mutate, restored together when the rebuild fails
type switchSnapshot struct {
	model, provider, requestedProvider, baseURL, apiMode, apiKey string
	client, anthropicClient                                      any
	anthropicAPIKey, anthropicBaseURL                            string
	anthropicTokenSource                                         *provider.TaggedTokenSource
	isAnthropicOAuth                                             bool
	configContextLength                                          int
	reasoningEchoFlag                                            any
	runtimeCapabilities                                          map[string]bool
	credentialPool                                               *provider.Pool
	credentialPoolEntryID                                        string
	codexReplayEnabled, codexReplayRejected                      bool
	clientKwargs                                                 map[string]any
}

func snapshotSwitchState(a *AIAgent) *switchSnapshot {
	return &switchSnapshot{model: a.model, provider: a.provider, requestedProvider: a.requestedProvider, baseURL: a.baseURL,
		apiMode: a.apiMode, apiKey: a.apiKey, client: a.client, anthropicClient: a.anthropicClient,
		anthropicAPIKey: a.anthropicAPIKey, anthropicBaseURL: a.anthropicBaseURL, anthropicTokenSource: a.anthropicTokenSource,
		isAnthropicOAuth: a.isAnthropicOAuth, configContextLength: a.configContextLength, reasoningEchoFlag: a.reasoningEchoFlag,
		runtimeCapabilities: a.runtimeCapabilities, credentialPool: a.credentialPool, credentialPoolEntryID: a.credentialPoolEntryID,
		codexReplayEnabled: a.codexReasoningReplayEnabled, codexReplayRejected: a.codexReasoningReplayRejected,
		clientKwargs: maps.Clone(a.clientKwargs)}
}

func restoreSwitchSnapshot(a *AIAgent, s *switchSnapshot) {
	a.model, a.provider, a.requestedProvider, a.apiMode, a.apiKey = s.model, s.provider, s.requestedProvider, s.apiMode, s.apiKey
	a.setBaseURL(s.baseURL)
	a.client, a.anthropicClient, a.anthropicAPIKey, a.anthropicBaseURL = s.client, s.anthropicClient, s.anthropicAPIKey, s.anthropicBaseURL
	a.anthropicTokenSource, a.isAnthropicOAuth, a.configContextLength = s.anthropicTokenSource, s.isAnthropicOAuth, s.configContextLength
	a.reasoningEchoFlag, a.runtimeCapabilities = s.reasoningEchoFlag, s.runtimeCapabilities
	a.credentialPool, a.credentialPoolEntryID = s.credentialPool, s.credentialPoolEntryID
	a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected = s.codexReplayEnabled, s.codexReplayRejected
	if s.clientKwargs == nil {
		a.clientKwargs = map[string]any{}
	} else {
		a.clientKwargs = s.clientKwargs
	}
}

// Runs fn and rolls the switch back to the snapshot when it fails or panics, passing the failure on
func rollbackSwitchOnFailure(a *AIAgent, s *switchSnapshot, fn func() error) error {
	defer func() {
		if r := recover(); r != nil {
			restoreSwitchSnapshot(a, s)
			panic(r)
		}
	}()
	if err := fn(); err != nil {
		restoreSwitchSnapshot(a, s)
		return err
	}
	return nil
}

// First value unless empty, Python's a or b over strings
func stringOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

var trailingV1 = regexp.MustCompile(`/v1/?$`)

// Api mode, base url and capability map of the switch destination
func resolveSwitchDestination(ctx context.Context, a *AIAgent, newModel, newProvider, baseURL, apiMode string, capabilities map[string]bool, oldNorm, newNorm string) (string, string, map[string]bool) {
	if apiMode == "" {
		apiMode = string(provider.DetermineModeCtx(ctx, newProvider, baseURL, newModel, nousAnthropicWire(ctx)))
	}
	if baseURL == "" && newNorm == "openai" {
		baseURL = "https://api.openai.com/v1"
	}
	effectiveBaseURL := baseURL
	if effectiveBaseURL == "" && oldNorm == newNorm {
		effectiveBaseURL = a.baseURL
	}
	if provider.IsActualRoute(newProvider, effectiveBaseURL) {
		apiMode = "chat_completions"
		if effectiveBaseURL != "" {
			baseURL = provider.NormalizeActualBaseURL(effectiveBaseURL)
		}
	}
	var destination map[string]bool
	if capabilities != nil {
		destination = maps.Clone(capabilities)
	} else {
		destination = map[string]bool{"native_compaction": provider.ResolveNativeCompactionCapability(newModel, effectiveBaseURL, newProvider, newNorm == "openai-codex")}
	}
	if apiMode == "anthropic_messages" && provider.OpenCodeFamily(newProvider) != "" && baseURL != "" {
		baseURL = trailingV1.ReplaceAllString(baseURL, "")
	}
	return apiMode, baseURL, destination
}

// Builds the MoA facade, Bedrock binding, native Anthropic client or OpenAI wire client for the destination
func buildSwitchedClient(ctx context.Context, a *AIAgent, newProvider, apiKey, baseURL, apiMode, newNorm string) error {
	if newNorm == "moa" {
		var key any
		if apiKey != "" {
			key = apiKey
		}
		return bindMoaRuntime(ctx, a, a.model, key)
	}
	if newProvider == "bedrock" && (apiMode == "anthropic_messages" || apiMode == "bedrock_converse") {
		return applyBedrockBinding(ctx, a, stringOr(baseURL, a.baseURL), provider.APIMode(apiMode))
	}
	timeout, hasTimeout := provider.GetProviderRequestTimeout(ctx, a.provider, a.model)
	if apiMode == "anthropic_messages" {
		effective := stringOr(apiKey, a.apiKey)
		if effective == "" && newProvider == "anthropic" {
			effective = resolveAnthropicToken(agentPoolEnv(ctx), a.model)
		}
		var key any = effective
		var tokenSource func(context.Context) (string, error)
		a.anthropicTokenSource = nil
		if newProvider == "minimax-oauth" && effective != "" {
			src := provider.BuildMinimaxOAuthTokenProvider(agentPoolEnv(ctx))
			a.anthropicTokenSource, key, tokenSource = src, src, src.Mint
		}
		a.apiKey, a.anthropicAPIKey = effective, effective
		a.anthropicBaseURL = stringOr(baseURL, a.anthropicBaseURL)
		client, err := anthropicClientBuilder(ctx, key, a.anthropicBaseURL, timeout, false)
		if err != nil {
			return err
		}
		a.anthropicClient = client
		a.isAnthropicOAuth = provider.AnthropicRouteIsOAuth(ctx, a.anthropicBaseURL, effective, tokenSource, newProvider)
		a.client, a.clientKwargs = nil, map[string]any{}
		return nil
	}
	effectiveBase := stringOr(baseURL, a.baseURL)
	a.clientKwargs = map[string]any{"api_key": stringOr(apiKey, a.apiKey), "base_url": effectiveBase}
	if cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv); err != nil {
		agentRuntimeHelpersLog().Debug("custom-provider TLS resolution skipped on switch_model")
	} else {
		provider.ApplyCustomProviderTLSToClientKwargs(a.clientKwargs, effectiveBase, provider.CompatibleCustomProviders(cfg), nil)
	}
	if hasTimeout {
		a.clientKwargs["timeout"] = timeout
	}
	if err := a.applyClientHeadersForBaseURL(ctx, effectiveBase, true); err != nil {
		return err
	}
	client, err := a.createOpenAIClient(ctx, maps.Clone(a.clientKwargs), "switch_model", true)
	if err != nil {
		return err
	}
	a.client = client
	return nil
}

// Swaps identity and transport fields, reloads the pool on a provider change and rebuilds the client
func swapSwitchRuntime(ctx context.Context, a *AIAgent, newModel, newProvider, apiKey, baseURL, apiMode, oldProvider, oldNorm, newNorm string) error {
	a.configContextLength = 0
	a.model = newModel
	a.provider, a.requestedProvider = newProvider, newProvider
	a.reasoningEchoFlag = readReasoningEchoFromConfig(ctx)
	if baseURL != "" {
		a.setBaseURL(baseURL)
	} else if oldNorm != newNorm {
		return fmt.Errorf("switch_model: no base_url resolved for provider '%s' (switching from '%s'); refusing to keep the previous provider's endpoint", newProvider, oldProvider)
	}
	a.apiMode = apiMode
	clear(a.transportCache)
	a.codexReasoningReplayEnabled, a.codexReasoningReplayRejected = true, false
	if apiKey != "" {
		a.apiKey = apiKey
	}
	if oldNorm != newNorm || a.credentialPool == nil {
		a.credentialPool, a.credentialPoolEntryID = nil, ""
		if err := guardPhase(func() error { a.credentialPool = agentLoadPool(agentPoolEnv(ctx), newProvider); return nil }); err != nil {
			agentRuntimeHelpersLog().Warn(fmt.Sprintf("switch_model: credential pool reload failed for %s (%s); continuing without pool rotation this turn", newProvider, err.Error()))
		}
	}
	if err := buildSwitchedClient(ctx, a, newProvider, apiKey, baseURL, apiMode, newNorm); err != nil {
		return err
	}
	syncCredentialPoolEntryID(a)
	return nil
}

// Destination context length with the LM Studio preload, and the live custom providers when config loaded
func resolveSwitchContextLength(ctx context.Context, a *AIAgent, snapshot *switchSnapshot) ([]map[string]any, bool, int, error) {
	var customProviders []map[string]any
	loaded := false
	var intent *int
	if cfg, err := config.Load(home.Dir(ctx), os.LookupEnv); err == nil {
		switchCfg := map[string]any(cfg)
		customProviders, loaded = provider.CompatibleCustomProviders(switchCfg), true
		intent = configContextLengthForRuntime(ctx, a, switchCfg)
		if intent == nil {
			if n := provider.CustomProviderContextLength(a.model, a.baseURL, customProviders, switchCfg); n != 0 {
				intent = &n
			}
		}
	}
	if err := setConfigContextLength(ctx, a, intent); err != nil {
		return nil, false, 0, err
	}
	var runtime *provider.LMStudioLoadResult
	if err := rollbackSwitchOnFailure(a, snapshot, func() error { runtime = a.ensureLMStudioRuntimeLoaded(ctx, intent); return nil }); err != nil {
		return nil, false, 0, err
	}
	if lmstudioLoadWasUnverified(runtime) {
		agentRuntimeHelpersLog().Warn("LM Studio model activation was rejected or completed without a verifiable active context length during model switch; continuing with configured context")
	}
	var intentValue any
	if intent != nil {
		intentValue = *intent
	}
	return customProviders, loaded, effectiveLMStudioContextLength(intentValue, runtime), nil
}

// Points the context compressor at the switched model and rechecks compression feasibility
func updateSwitchCompressor(ctx context.Context, a *AIAgent, customProviders []map[string]any, loaded bool, effectiveContextLength int, snapshot *switchSnapshot) error {
	if !loaded {
		if cfg, err := config.Load(home.Dir(ctx), os.LookupEnv); err == nil {
			customProviders = provider.CompatibleCustomProviders(cfg)
		}
	}
	probeKey := a.apiKey
	if a.anthropicTokenSource != nil {
		probeKey = ""
	}
	if err := rollbackSwitchOnFailure(a, snapshot, func() error {
		n := switchModelContextLength(ctx, provider.ContextLengthRequest{Model: a.model, BaseURL: a.baseURL, APIKey: probeKey,
			Provider: a.provider, ConfigContextLength: effectiveContextLength, CustomProviders: customProviders})
		a.contextCompressor.UpdateModel(a.model, n, a.baseURL, a.apiKey, a.provider, a.apiMode)
		return nil
	}); err != nil {
		return err
	}
	revalidateCompressionFeasibility(ctx, a)
	return nil
}

// Resets the fallback walk, prunes chain entries naming either primary and rederives request overrides
func finishSwitch(ctx context.Context, a *AIAgent, newProvider, oldNorm, newNorm string) {
	a.fallbackActivated, a.providerFallbackActive = false, false
	a.providerFallbackRoute = [2]string{}
	a.fallbackIndex = 0
	a.credentialPoolRevertID = ""
	chain := append([]map[string]any(nil), a.fallbackChain...)
	if oldNorm != "" && newNorm != "" && oldNorm != newNorm {
		kept := chain[:0:0]
		for _, entry := range chain {
			if p := pystr.Lower(pystr.Strip(pyStrOr(entry["provider"], ""))); p != oldNorm && p != newNorm {
				kept = append(kept, entry)
			}
		}
		chain = kept
	}
	a.fallbackChain = chain
	if err := guardPhase(func() error { applySwitchedProviderRequestOverrides(ctx, a, newProvider); return nil }); err != nil {
		agentRuntimeHelpersLog().Debug("switch_model: request_overrides re-derivation failed")
	}
}

// Replaces the request overrides extra_body with the switched provider's custom entry extra_body
func applySwitchedProviderRequestOverrides(ctx context.Context, a *AIAgent, newProvider string) {
	customProviders := a.customProviderEntries
	if a.customProviders == nil {
		customProviders = nil
		if cfg, err := config.Load(home.Dir(ctx), os.LookupEnv); err == nil {
			customProviders = provider.CompatibleCustomProviders(cfg)
		}
	}
	newExtraBody := customProviderExtraBodyForAgent(newProvider, a.model, a.baseURL, customProviders)
	overrides := maps.Clone(a.requestOverrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	delete(overrides, "extra_body")
	if len(newExtraBody) > 0 {
		overrides["extra_body"] = maps.Clone(newExtraBody)
	}
	a.requestOverrides = overrides
}

// Records the post switch provider, endpoint and api mode as the session billing route
func persistSwitchBillingRoute(ctx context.Context, a *AIAgent) {
	if a.sessionDB == nil || a.sessionID == "" {
		return
	}
	mode := a.apiMode
	if err := a.sessionDB.UpdateSessionBillingRoute(ctx, a.sessionID, a.provider, a.baseURL, &mode); err != nil {
		agentRuntimeHelpersLog().Warn("Failed to persist billing route after model switch", "error", err)
	}
}

// Switches model, provider and api mode in place and persists the route across turns, rolling back a failed rebuild
func (a *AIAgent) switchModel(ctx context.Context, newModel, newProvider, apiKey, baseURL, apiMode string, capabilities map[string]bool) error {
	log := agentRuntimeHelpersLog()
	oldModel, oldProvider := a.model, a.provider
	oldNorm, newNorm := pystr.Lower(pystr.Strip(oldProvider)), pystr.Lower(pystr.Strip(newProvider))
	apiMode, baseURL, destination := resolveSwitchDestination(ctx, a, newModel, newProvider, baseURL, apiMode, capabilities, oldNorm, newNorm)
	snapshot := snapshotSwitchState(a)
	if err := rollbackSwitchOnFailure(a, snapshot, func() error {
		return swapSwitchRuntime(ctx, a, newModel, newProvider, apiKey, baseURL, apiMode, oldProvider, oldNorm, newNorm)
	}); err != nil {
		return err
	}
	customProviders, loaded, effectiveContextLength, err := resolveSwitchContextLength(ctx, a, snapshot)
	if err != nil {
		return err
	}
	if loaded {
		a.customProviderEntries = customProviders
		a.customProviders = make([]any, len(customProviders))
		for i, entry := range customProviders {
			a.customProviders[i] = entry
		}
	}
	a.usePromptCaching, a.useNativeCacheLayout = provider.ResolveAnthropicPromptCachePolicy(ctx, &provider.CachePolicyAgent{
		Provider: a.provider, BaseURL: a.baseURL, APIMode: a.apiMode, Model: a.model,
		CacheDisabled: a.cacheDisabled, CustomProviders: a.customProviderEntries,
	}, provider.CacheDestination{Provider: &newProvider, BaseURL: &a.baseURL, APIMode: &apiMode, Model: &newModel})
	if a.contextCompressor != nil {
		if err := updateSwitchCompressor(ctx, a, customProviders, loaded, effectiveContextLength, snapshot); err != nil {
			return err
		}
	}
	if cfg, err := config.Load(home.Dir(ctx), os.LookupEnv); err != nil {
		log.Debug(fmt.Sprintf("switch_model: could not re-resolve reasoning_config: %s", err.Error()))
	} else {
		a.reasoningConfig = config.ResolveReasoningConfig(map[string]any(cfg), a.model)
		log.Info(fmt.Sprintf("switch_model: reasoning_config resolved for %s: %s", a.model, pyrepr.Repr(a.reasoningConfig)))
	}
	a.cachedSystemPrompt = ""
	a.runtimeCapabilities = destination
	a.RetrySession().ResetStaleStreak()
	a.primaryRuntime = buildPrimaryRuntimeSnapshot(ctx, a, apiMode)
	finishSwitch(ctx, a, newProvider, oldNorm, newNorm)
	log.Info(fmt.Sprintf("Model switched in-place: %s (%s) -> %s (%s)", oldModel, oldProvider, newModel, newProvider))
	persistSwitchBillingRoute(ctx, a)
	return nil
}
