package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycopy"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func chatCompletionHelpersLog() *slog.Logger {
	return slog.Default().With("logger", "agent.chat_completion_helpers")
}

// Agent state of the request builders, the reasoning capability probes and the fallback chain walk
type apiKwargsState struct {
	reasoningProbeMu     sync.Mutex
	lmReasoningOptsCache map[[2]string]reasoningProbeEntry
}

// Hostname of the url is the domain or one of its subdomains, a malformed url raising like Python
func hostMatches(url, domain string) bool {
	ok, err := pyurl.BaseURLHostMatches(url, domain)
	if err != nil {
		panic(err)
	}
	return ok
}

// Python None for an unset optional integer
// Python value of an optional int, None when unset and zero kept as zero
func intPtrOrNone(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// Python None for a zero int
func intOrNone(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// Python None for an unset optional dict
func dictOrNone(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// AttributeError a missing transport raises on build_kwargs
func buildWithTransport(t provider.Transport, model string, messages, tools []map[string]any, params map[string]any) (map[string]any, error) {
	if t == nil {
		return nil, fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'build_kwargs'")
	}
	return t.BuildKwargs(model, messages, tools, params)
}

// Tool definitions of the agent registry list as request dicts, nil when the agent has none
func agentToolDicts(list []any) []map[string]any {
	if list == nil {
		return nil
	}
	return mapRows(list)
}

// Request kwargs of the active api mode carrying the conversation affinity headers on every transport
func (a *AIAgent) buildAPIKwargs(apiMessages []map[string]any, toolsForAPI []map[string]any, toolsOverride bool) (map[string]any, error) {
	ctx := a.homeContext()
	kwargs, err := a.buildAPIKwargsForMode(ctx, apiMessages, toolsForAPI, toolsOverride)
	if err != nil {
		return nil, err
	}
	return provider.MergeSessionAffinityHeaders(ctx, kwargs, a.provider, a.baseURL, a.sessionID), nil
}

// Dispatches to the api mode builder with the wire reasoning config and the request overrides consumed once
func (a *AIAgent) buildAPIKwargsForMode(ctx context.Context, apiMessages []map[string]any, toolsForAPI []map[string]any, toolsOverride bool) (map[string]any, error) {
	reasoningConfig := provider.ReasoningConfigForWire(ctx, a)
	if !toolsOverride {
		toolsForAPI = agentToolDicts(a.tools)
	}
	requestOverrides := a.EffectiveRequestOverrides()
	switch a.apiMode {
	case "anthropic_messages":
		return a.buildAnthropicKwargs(ctx, apiMessages, toolsForAPI, reasoningConfig, requestOverrides)
	case "bedrock_converse":
		return a.buildBedrockKwargs(apiMessages, toolsForAPI, reasoningConfig)
	}
	cacheScopeID := a.promptCacheScopeForAgent()
	if a.apiMode == "codex_responses" {
		return a.buildCodexKwargs(ctx, apiMessages, toolsForAPI, reasoningConfig, requestOverrides, cacheScopeID)
	}
	return a.buildChatCompletionsKwargs(ctx, apiMessages, toolsForAPI, reasoningConfig, requestOverrides, cacheScopeID)
}

// Pops the one shot output cap so whichever builder runs consumes it
func (a *AIAgent) consumeEphemeralMaxOutput() any {
	out := a.ephemeralMaxOutputTokens
	if out != 0 {
		a.ephemeralMaxOutputTokens = 0
	}
	return intOrNone(out)
}

// Rotation stable logical cache scope, nil so transports fall back to the physical session id
func (a *AIAgent) promptCacheScopeForAgent() any {
	if scope := provider.ResolvePromptCacheScopeSafe(a.cacheScopeAgent()); scope != "" {
		return scope
	}
	return nil
}

// Validated provider routing object with the per model provider_routing overlay for the current model
func (a *AIAgent) providerPreferencesForAgent(ctx context.Context) map[string]any {
	keys := []string{"only", "ignore", "order", "sort", "require_parameters", "data_collection"}
	merged := map[string]any{"only": a.providersAllowed, "ignore": a.providersIgnored, "order": a.providersOrder,
		"sort": a.providerSort, "require_parameters": a.providerRequireParameters, "data_collection": a.providerDataCollection}
	perModel := map[string]any{}
	func() {
		defer func() { _ = recover() }()
		cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
		if err != nil {
			return
		}
		var models any
		switch pr := cfg["provider_routing"].(type) {
		case map[string]any:
			models = pr["models"]
		case config.Tree:
			models = pr["models"]
		}
		perModel = config.ResolvePerModelProviderRouting(a.model, models)
	}()
	for _, k := range keys {
		if v, ok := perModel[k]; ok {
			merged[k] = v
		}
	}
	if sort, ok := provider.ValidatedOpenRouterProviderSort(merged["sort"]); ok {
		merged["sort"] = sort
	} else {
		merged["sort"] = nil
	}
	if pyrepr.Truthy(merged["require_parameters"]) {
		merged["require_parameters"] = true
	} else {
		merged["require_parameters"] = nil
	}
	out := map[string]any{}
	for _, k := range keys {
		if pyrepr.Truthy(merged[k]) {
			out[k] = merged[k]
		}
	}
	return out
}

// Renames the xAI reserved tool_search bridge on the wire and records the alias map the normalizer reverses
func (a *AIAgent) aliasToolSearchBridgeForXAI(t provider.Transport, toolsForAPI []map[string]any) []map[string]any {
	aliasing, canAlias := t.(provider.WireAliasTransport)
	if canAlias {
		aliasing.SetLastWireAliases(map[string]string{})
	}
	isXAIChat := a.provider == "xai" || a.provider == "xai-oauth" || a.baseURLHostname == "api.x.ai"
	if !isXAIChat || len(toolsForAPI) == 0 {
		return toolsForAPI
	}
	hasBridge := false
	for _, tool := range toolsForAPI {
		if tool == nil {
			continue
		}
		fn := tool["function"]
		if !pyrepr.Truthy(fn) {
			continue
		}
		fnMap, ok := fn.(map[string]any)
		if !ok {
			chatCompletionHelpersLog().Warn(fmt.Sprintf("%s⚠️ Failed to alias tool_search bridge for xAI: '%s' object has no attribute 'get'", a.logPrefix, pyrepr.TypeName(fn)))
			return toolsForAPI
		}
		if fnMap["name"] == "tool_search" {
			hasBridge = true
		}
	}
	if !hasBridge {
		return toolsForAPI
	}
	copied, _ := pycopy.DeepCopy(toolsForAPI).([]map[string]any)
	renamed, aliases := provider.RenameToolSearchBridgeForXAI(copied)
	if canAlias {
		aliasing.SetLastWireAliases(aliases)
	}
	return renamed
}

// Messages kwargs with the Nous Portal profile body merged for sticky routing
func (a *AIAgent) buildAnthropicKwargs(ctx context.Context, apiMessages, toolsForAPI []map[string]any, reasoningConfig, requestOverrides map[string]any) (map[string]any, error) {
	var contextLength any
	if a.contextCompressor != nil {
		contextLength = engineContextLength(a.contextCompressor)
	}
	maxTokens := a.consumeEphemeralMaxOutput()
	if maxTokens == nil {
		maxTokens = intPtrOrNone(a.maxTokens)
	}
	t := a.getTransport("")
	prepared, err := a.prepareAnthropicMessagesForAPI(ctx, anyRows(apiMessages))
	if err != nil {
		return nil, err
	}
	kwargs, err := buildWithTransport(t, a.model, mapRows(prepared), toolsForAPI, map[string]any{
		"max_tokens": maxTokens, "reasoning_config": dictOrNone(reasoningConfig), "is_oauth": a.isAnthropicOAuth,
		"preserve_dots": a.anthropicPreserveDots(), "context_length": contextLength, "base_url": a.anthropicBaseURL,
		"fast_mode": requestOverrides["speed"] == "fast", "drop_context_1m_beta": a.oauth1MBetaDisabled,
	})
	if err != nil {
		return nil, err
	}
	return provider.MergeNousPortalMessagesExtraBody(ctx, a, kwargs, apiMessages), nil
}

// Converse kwargs, the adapter converting messages and tools itself
func (a *AIAgent) buildBedrockKwargs(apiMessages, toolsForAPI []map[string]any, reasoningConfig map[string]any) (map[string]any, error) {
	region := a.bedrockRegion
	if region == "" {
		region = "us-east-1"
	}
	return buildWithTransport(a.getTransport(""), a.model, apiMessages, toolsForAPI, map[string]any{
		"max_tokens": intPtrOrNone(a.maxTokens), "region": region, "guardrail_config": dictOrNone(a.bedrockGuardrailConfig),
		"reasoning_config": dictOrNone(reasoningConfig),
	})
}

// Native compaction payload of the Responses route from the agent gates, nil on every ineligible route
func (a *AIAgent) nativeCompactionContextManagement(route provider.ResponsesRoute) []map[string]any {
	var localTrigger, threshold any
	if a.contextCompressor != nil {
		localTrigger = engineThresholdTokens(a.contextCompressor)
	}
	if a.codexResponsesCompactThreshold != 0 {
		threshold = a.codexResponsesCompactThreshold
	}
	return provider.NativeCompactionContextManagement(provider.NativeCompactionGate{
		RuntimeCapability: a.runtimeCapabilities == nil || a.runtimeCapabilities["native_compaction"],
		Configured:        a.codexResponsesNativeCompaction, CompressionEnabled: a.compressionEnabled,
		CheckpointRequired: a.compressionCheckpointRequired, TrustedProxy: a.capabilities["openai_native_compaction"],
		Model: a.model, Provider: a.provider, BaseURL: a.baseURL, LocalTriggerTokens: localTrigger, CompactThreshold: threshold,
	}, route)
}

// Responses kwargs with native compaction and the xAI schema keyword strip on a deep copy of the tools
func (a *AIAgent) buildCodexKwargs(ctx context.Context, apiMessages, toolsForAPI []map[string]any, reasoningConfig, requestOverrides map[string]any, cacheScopeID any) (map[string]any, error) {
	route := provider.ClassifyResponsesRoute(a.provider, a.baseURL)
	contextManagement := a.nativeCompactionContextManagement(route)
	if route.IsXAI() {
		copied, _ := pycopy.DeepCopy(toolsForAPI).([]map[string]any)
		copied, _ = tools.StripPatternAndFormat(copied)
		toolsForAPI, _ = tools.StripSlashEnum(copied)
	}
	maxTokens := a.consumeEphemeralMaxOutput()
	if maxTokens == nil {
		maxTokens = intPtrOrNone(a.maxTokens)
	}
	t := a.getTransport("")
	prepared, err := a.prepareMessagesForNonVisionModel(ctx, anyRows(apiMessages))
	if err != nil {
		return nil, err
	}
	var profileReasoning any
	if route.ProfileReasoning() {
		profileReasoning = a.endpointReasoningPayload(ctx)
	}
	return buildWithTransport(t, a.model, mapRows(prepared), toolsForAPI, map[string]any{
		"reasoning_config": dictOrNone(reasoningConfig), "session_id": a.sessionID, "cache_scope_id": cacheScopeID,
		"base_url": a.baseURL, "max_tokens": maxTokens, "timeout": a.resolvedAPICallTimeout(ctx),
		"request_overrides": requestOverrides, "provider": a.provider, "is_codex_backend": route.IsCodexBackend(),
		"is_xai_responses": route.IsXAI(), "profile_reasoning": profileReasoning,
		"replay_encrypted_reasoning": a.codexReasoningReplayEnabled, "context_management": contextManagement,
		"text_verbosity": a.textVerbosity,
	})
}

// Omit flag and fixed temperature of the model, both unset when the lookup fails
func fixedTemperatureFor(model, baseURL string) (omit bool, fixed any) {
	defer func() {
		if recover() != nil {
			omit, fixed = false, nil
		}
	}()
	o, f, err := provider.FixedTemperatureForModel(model, baseURL, "")
	if err != nil {
		return false, nil
	}
	if o {
		return true, nil
	}
	if f != nil {
		return false, *f
	}
	return false, nil
}

// Registered provider profile of the agent's provider, nil when none resolves
func (a *AIAgent) chatProviderProfile(ctx context.Context) (out *provider.Profile) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	if p, ok := provider.GetProviderProfile(ctx, a.provider); ok {
		return &p
	}
	return nil
}

// Chat completions kwargs through the provider profile hooks, or the legacy route flags for unregistered providers
func (a *AIAgent) buildChatCompletionsKwargs(ctx context.Context, apiMessages, toolsForAPI []map[string]any, reasoningConfig, requestOverrides map[string]any, cacheScopeID any) (map[string]any, error) {
	t := a.getTransport("")
	toolsForAPI = a.aliasToolSearchBridgeForXAI(t, toolsForAPI)
	isQwen := a.isQwenPortal()
	isOpenRouter := a.isOpenRouterURL()
	host := a.baseURLLower
	isLMStudio := pystr.Lower(pystr.Strip(a.provider)) == "lmstudio"
	profileEndpoint := a.reasoningEndpointProfile(ctx) != nil
	omitTemp, fixedTemp := fixedTemperatureFor(a.model, a.baseURL)
	prefs := a.providerPreferencesForAgent(ctx)
	var qwenMeta any
	if isQwen {
		sid := a.sessionID
		if sid == "" {
			sid = "icebath"
		}
		qwenMeta = map[string]any{"sessionId": sid, "promptId": uuid.NewString()}
	}
	profile := a.chatProviderProfile(ctx)
	ephemeral := a.consumeEphemeralMaxOutput()
	apiMessages = provider.ShapeWireCarriers(ctx, apiMessages, a.model, a.baseURL, a)
	prepared, err := a.prepareMessagesForNonVisionModel(ctx, anyRows(apiMessages))
	if err != nil {
		return nil, err
	}
	var prefsOrNone, lmOptions any
	if len(prefs) > 0 {
		prefsOrNone = prefs
	}
	params := map[string]any{
		"base_url": a.baseURL, "timeout": a.resolvedAPICallTimeout(ctx), "max_tokens": intPtrOrNone(a.maxTokens),
		"ephemeral_max_output_tokens": ephemeral, "max_tokens_param_fn": func(n int) map[string]any { return a.maxTokensParam(ctx, n) },
		"reasoning_config": dictOrNone(reasoningConfig), "request_overrides": requestOverrides, "session_id": a.sessionID,
		"cache_scope_id": cacheScopeID, "ollama_num_ctx": intOrNone(a.ollamaNumCtx), "provider_preferences": prefsOrNone,
		"openrouter_min_coding_score": a.openrouterMinCodingScore, "supports_reasoning": a.supportsReasoningExtraBody(ctx),
	}
	if isLMStudio {
		lmOptions = a.lmstudioReasoningOptionsCached(ctx)
	}
	params["lmstudio_reasoning_options"] = lmOptions
	params["qwen_session_metadata"] = qwenMeta
	messages := mapRows(prepared)
	if profile != nil {
		params["provider_profile"] = profile
		return buildWithTransport(t, a.model, messages, toolsForAPI, params)
	}
	isKimi := false
	for _, h := range []string{"api.kimi.com", "moonshot.ai", "moonshot.cn"} {
		if hostMatches(a.baseURL, h) {
			isKimi = true
			break
		}
	}
	var qwenPrepare, qwenPrepareInplace, endpointReasoning any
	if isQwen {
		qwenPrepare, qwenPrepareInplace = a.qwenPrepareChatMessages, a.qwenPrepareChatMessagesInplace
	}
	if profileEndpoint {
		endpointReasoning = a.endpointReasoningPayload(ctx)
	}
	for k, v := range map[string]any{
		"model_lower": pystr.Lower(a.model), "is_openrouter": isOpenRouter, "is_nous": hostMatches(host, "nousresearch.com"),
		"is_qwen_portal": isQwen, "is_nvidia_nim": hostMatches(host, "integrate.api.nvidia.com"), "is_kimi": isKimi,
		"is_tokenhub": hostMatches(host, "tokenhub.tencentmaas.com"), "is_custom_provider": a.provider == "custom",
		"qwen_prepare_fn": qwenPrepare, "qwen_prepare_inplace_fn": qwenPrepareInplace, "fixed_temperature": fixedTemp,
		"omit_temperature": omitTemp, "profile_endpoint": profileEndpoint, "endpoint_reasoning": endpointReasoning,
		"provider_name": a.provider,
	} {
		params[k] = v
	}
	return buildWithTransport(t, a.model, messages, toolsForAPI, params)
}
