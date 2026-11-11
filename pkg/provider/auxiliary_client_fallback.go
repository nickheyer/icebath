package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Positive float of a config timeout, false for bools, non numbers and non positive values
func auxCoercePositiveTimeout(raw any) (float64, bool) {
	var f float64
	switch v := raw.(type) {
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	case float64:
		f = v
	case float32:
		f = float64(v)
	default:
		return 0, false
	}
	if f > 0 {
		return f, true
	}
	return 0, false
}

// Positive float timeout of a raw config value, false for missing, non numeric or non positive values
func CoercePositiveTimeout(raw any) (float64, bool) { return auxCoercePositiveTimeout(raw) }

// Timeout a configured fallback chain entry declares for itself, false to keep the task timeout
func auxFallbackEntryTimeout(ctx context.Context, task, fbLabel string) (float64, bool) {
	entry := auxFallbackChainEntry(ctx, task, fbLabel)
	if entry == nil {
		return 0, false
	}
	return auxCoercePositiveTimeout(entry["timeout"])
}

type auxFallbackDestination struct {
	Provider string
	BaseURL  string
	APIMode  string
	Model    string
}

// Destination with its api mode filled from the endpoint shape or the runtime resolver when unset
func auxCompleteFallbackDestination(ctx context.Context, provider, baseURL, apiMode, model string) (auxFallbackDestination, error) {
	if apiMode == "" {
		speaks, err := auxEndpointSpeaksAnthropicMessages(baseURL)
		if err != nil {
			return auxFallbackDestination{}, err
		}
		if speaks {
			apiMode = "anthropic_messages"
		} else if rt, err := ResolveRuntimeProvider(ctx, home.Dir(ctx), provider, model, baseURL, ""); err == nil && rt != nil {
			apiMode = strings.TrimSpace(string(rt.APIMode))
		}
	}
	return auxFallbackDestination{Provider: provider, BaseURL: baseURL, APIMode: apiMode, Model: model}, nil
}

// Python str of a truthy value, empty for a falsy one
func auxStrOr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Python str of a present key, empty when the key is absent
func auxStrGet(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	return pyrepr.Str(v)
}

// Route identity of a configured fallback entry, the client base URL standing in for a missing one
func auxFallbackDestinationFromEntry(ctx context.Context, entry map[string]any, fbClient AuxClient, fbModel string) (auxFallbackDestination, error) {
	provider := strings.TrimSpace(auxStrOr(entry["provider"]))
	baseURL := auxStrOr(entry["base_url"])
	if baseURL == "" && fbClient != nil {
		baseURL = fbClient.BaseURL()
	}
	apiMode := auxStrOr(entry["api_mode"])
	if apiMode == "" {
		apiMode = auxStrOr(entry["transport"])
	}
	model := fbModel
	if model == "" {
		model = strings.TrimSpace(auxStrOr(entry["model"]))
	}
	return auxCompleteFallbackDestination(ctx, provider, strings.TrimSpace(baseURL), strings.TrimSpace(apiMode), model)
}

// Route identity of a fallback request from the attached destination, else the configured entry, else the label
func auxFallbackDestinationFor(ctx context.Context, task string, fbClient AuxClient, fbModel, fbLabel string) (auxFallbackDestination, error) {
	if holder, ok := fbClient.(auxFallbackDestHolder); ok {
		if d, attached := holder.auxFallbackDest(); attached {
			return d, nil
		}
	}
	if entry := auxFallbackChainEntry(ctx, task, fbLabel); entry != nil {
		return auxFallbackDestinationFromEntry(ctx, entry, fbClient, fbModel)
	}
	baseURL := ""
	if fbClient != nil {
		baseURL = fbClient.BaseURL()
	}
	return auxCompleteFallbackDestination(ctx, auxFallbackProviderFromLabel(fbLabel), baseURL, "", fbModel)
}

// Cache sections planned for one destination at the configured TTL, one shot requests never cached
func auxReplanSynchronousCacheSections(ctx context.Context, messages, tools []map[string]any, dest auxFallbackDestination) ([]map[string]any, []map[string]any) {
	var cacheDisabled *bool
	if isOneShotRequest(messages, tools) {
		disabled := true
		cacheDisabled = &disabled
	}
	return planCacheSectionsForDestination(ctx, messages, tools, dest.Provider, dest.BaseURL, dest.APIMode, dest.Model,
		cacheDisabled, configuredCacheTTL(ctx), nil)
}

// Request fields a fallback candidate call shares with the primary request
type auxFallbackRequest struct {
	Task               string
	Messages           []map[string]any
	Tools              []map[string]any
	Temperature        *float64
	MaxTokens          any
	EffectiveTimeout   float64
	EffectiveExtraBody map[string]any
	ReasoningConfig    map[string]any
}

// Wire messages or tools of a kwargs value as dicts, nil for a value that holds none
func auxDictList(v any) []map[string]any {
	switch list := v.(type) {
	case []map[string]any:
		return list
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// Request kwargs for one fallback destination with the cache sections replanned and the fast lane cap applied
func auxFallbackRequestKwargs(ctx context.Context, dest auxFallbackDestination, req auxFallbackRequest, fallbackEntry, taskConfig map[string]any, applyFastLane bool) (map[string]any, error) {
	maxTokens, extraBody := req.MaxTokens, req.EffectiveExtraBody
	if applyFastLane {
		var err error
		maxTokens, extraBody, err = auxCompressionFastLaneControls(ctx, req.Task, dest.Provider, dest.Model,
			auxStrOr(fallbackEntry["provider"]), auxStrOr(fallbackEntry["model"]), fallbackEntry, taskConfig, req.MaxTokens, req.EffectiveExtraBody)
		if err != nil {
			return nil, err
		}
	}
	messages, tools := auxReplanSynchronousCacheSections(ctx, req.Messages, req.Tools, dest)
	var toolsArg any
	if tools != nil {
		toolsArg = tools
	}
	return auxBuildCallKwargs(ctx, dest.Provider, dest.Model, messages, auxCallKwargsOptions{
		Temperature: req.Temperature, MaxTokens: maxTokens, Tools: toolsArg, Timeout: req.EffectiveTimeout,
		ExtraBody: extraBody, ReasoningConfig: req.ReasoningConfig, BaseURL: dest.BaseURL, Task: req.Task})
}

type auxFallbackRebuild func(provider string, client AuxClient, model string) (auxFallbackDestination, map[string]any, error)

// Destination, first attempt kwargs and the credential retry rebuild of a fallback candidate
func auxPlanFallbackCandidate(ctx context.Context, fbClient AuxClient, fbModel, fbLabel string, req auxFallbackRequest, applyFastLane bool) (auxFallbackDestination, map[string]any, auxFallbackRebuild, error) {
	if fbTimeout, ok := auxFallbackEntryTimeout(ctx, req.Task, fbLabel); ok && fbTimeout != req.EffectiveTimeout {
		slog.Info(fmt.Sprintf("Auxiliary %s: %s using its configured timeout %.0fs (task-level was %.0fs)",
			auxTaskOrCall(req.Task), fbLabel, fbTimeout, req.EffectiveTimeout))
		req.EffectiveTimeout = fbTimeout
	}
	dest, err := auxFallbackDestinationFor(ctx, req.Task, fbClient, fbModel, fbLabel)
	if err != nil {
		return dest, nil, nil, err
	}
	taskConfig := map[string]any{}
	if req.Task == "compression" {
		if taskConfig, err = auxGetAuxiliaryTaskConfig(ctx, req.Task); err != nil {
			return dest, nil, nil, err
		}
	}
	fallbackEntry := auxFallbackChainEntry(ctx, req.Task, fbLabel)
	if fallbackEntry == nil {
		fallbackEntry = map[string]any{}
	}
	rebuild := func(provider string, client AuxClient, model string) (auxFallbackDestination, map[string]any, error) {
		retry := auxFallbackDestination{Provider: provider, BaseURL: dest.BaseURL, APIMode: dest.APIMode, Model: model}
		if retry.BaseURL == "" && client != nil {
			retry.BaseURL = client.BaseURL()
		}
		if retry.Model == "" {
			retry.Model = dest.Model
		}
		kwargs, err := auxFallbackRequestKwargs(ctx, retry, req, fallbackEntry, taskConfig, applyFastLane)
		return retry, kwargs, err
	}
	kwargs, err := auxFallbackRequestKwargs(ctx, dest, req, fallbackEntry, taskConfig, applyFastLane)
	return dest, kwargs, rebuild, err
}

// Marks a candidate that cannot serve this walk unhealthy, transient reasons briefly and dead tokens for the long hold
func auxQuarantineFallbackCandidate(ctx context.Context, task, fbLabel, fbProvider string, fbErr error, baseURL, tag, reason string) {
	ttl := auxUnhealthyTTLSeconds
	if d, ok := auxFallbackCandidateQuarantineTTL(reason); ok {
		ttl = d.Seconds()
	}
	target := fbProvider
	if target == "" {
		target = fbLabel
	}
	markReason := reason
	if markReason == "" {
		markReason = "stale fallback credential"
	}
	auxMarkProviderUnhealthy(ctx, target, ttl, baseURL, markReason, slog.LevelWarn)
	why := "has a stale/unrefreshable credential"
	if reason != "" {
		why = fmt.Sprintf("is out of capacity (%s)", reason)
	}
	slog.Warn(fmt.Sprintf("Auxiliary %s%s: fallback candidate %s %s (%v) — skipping to next fallback", auxTaskOrCall(task), tag, fbLabel, why, fbErr))
}

type auxFallbackRetry struct {
	Client AuxClient
	Kwargs map[string]any
	Dest   auxFallbackDestination
}

// Refreshes the candidate's credentials after an auth error and rebuilds its request, nil retry when nothing refreshed
func auxPlanFallbackAuthRetry(ctx context.Context, dest auxFallbackDestination, rebuild auxFallbackRebuild, asyncMode bool, failedAPIKey string) (string, *auxFallbackRetry, error) {
	fbProvider := auxAuthRefreshProviderForRoute(ctx, dest.Provider, dest.BaseURL, "")
	key := ""
	if fbProvider == "anthropic" {
		key = failedAPIKey
	}
	if fbProvider != "auto" && fbProvider != "" && auxRefreshProviderCredentialsFn(ctx, fbProvider, key) {
		client, model, err := auxGetCachedClientFn(ctx, auxCachedClientRequest{Provider: fbProvider, Model: dest.Model, AsyncMode: asyncMode,
			BaseURL: dest.BaseURL, APIMode: dest.APIMode})
		if err != nil {
			return fbProvider, nil, err
		}
		if client != nil {
			retryDest, kwargs, err := rebuild(fbProvider, client, model)
			if err != nil {
				return fbProvider, nil, err
			}
			return fbProvider, &auxFallbackRetry{Client: client, Kwargs: kwargs, Dest: retryDest}, nil
		}
	}
	return fbProvider, nil, nil
}

// API key text of a client for pool and refresh bookkeeping, empty for a callable key
func auxClientAPIKeyText(client AuxClient) string {
	if client == nil {
		return ""
	}
	return client.APIKey()
}

// Calls one fallback candidate with one credential refresh retry, nil response when it was quarantined
func auxCallFallbackCandidate(ctx context.Context, fbClient AuxClient, fbModel, fbLabel string, req auxFallbackRequest, asyncMode bool) (any, error) {
	dest, fbKwargs, rebuild, err := auxPlanFallbackCandidate(ctx, fbClient, fbModel, fbLabel, req, !asyncMode)
	if err != nil {
		return nil, err
	}
	task := req.Task
	tag := ""
	if asyncMode {
		tag = " (async)"
	}
	sendRecovering := func(ctx context.Context, client AuxClient, kwargs map[string]any, d auxFallbackDestination) (any, error) {
		send := func(ctx context.Context, c AuxClient, kw map[string]any) (any, error) {
			var resp any
			var err error
			if asyncMode {
				resp, err = auxRelayAsyncCompletion(ctx, c, kw, d.Provider, d.APIMode, nil)
			} else {
				forceStream := auxProviderRequiresStream(ctx, d.BaseURL)
				resp, err = auxRelaySyncCompletionFn(ctx, c, kw, d.Provider, d.APIMode, func(ctx context.Context, request map[string]any) (any, error) {
					return auxCreateWithProgress(ctx, c, request, task, forceStream)
				})
			}
			if err != nil {
				return nil, err
			}
			return auxValidateLLMResponseFn(ctx, resp, task, "", "")
		}
		if asyncMode {
			return SendWithParameterRungsAsync(ctx, send, client, kwargs, task)
		}
		return SendWithParameterRungs(ctx, send, client, kwargs, task)
	}
	resp, fbErr := sendRecovering(ctx, fbClient, fbKwargs, dest)
	if fbErr == nil {
		return resp, nil
	}
	if !auxIsAuthError(fbErr) {
		capacity, ok := auxFallbackCandidateUnavailableReason(fbErr)
		if !ok {
			return nil, fbErr
		}
		auxQuarantineFallbackCandidate(ctx, task, fbLabel, dest.Provider, fbErr, dest.BaseURL, tag, capacity)
		return nil, nil
	}
	fbProvider, retry, err := auxPlanFallbackAuthRetry(ctx, dest, rebuild, asyncMode, auxClientAPIKeyText(fbClient))
	if err != nil {
		return nil, err
	}
	failed := dest
	if retry != nil {
		failed = retry.Dest
		resp, retryErr := sendRecovering(ctx, retry.Client, retry.Kwargs, retry.Dest)
		if retryErr == nil {
			return resp, nil
		}
		if _, capacity := auxFallbackCandidateUnavailableReason(retryErr); !auxIsAuthError(retryErr) && !capacity {
			return nil, retryErr
		}
	}
	auxQuarantineFallbackCandidate(ctx, task, fbLabel, fbProvider, fbErr, failed.BaseURL, tag, "")
	return nil, nil
}

// Discovery chain after a payment or connection error, skipping the failed provider and its backend
func auxTryPaymentFallback(ctx context.Context, failedProvider, task, reason, failedBaseURL string, scope FailureScope, mainRuntime map[string]any) (AuxClient, string, string, error) {
	skip := strings.TrimSpace(strings.ToLower(failedProvider))
	mainProvider, _ := auxNormalizeMainRuntime(ctx, mainRuntime)["provider"].(string)
	if mainProvider == "" {
		mainProvider = auxReadMainProvider(ctx)
	}
	if !auxDiscoveryChainAllowed(mainProvider, task) {
		return nil, "", "", nil
	}
	skipLabels := map[string]bool{skip: true}
	if mainProvider != "" && strings.Contains(skip, strings.ToLower(mainProvider)) {
		skipLabels[strings.ToLower(mainProvider)] = true
	}
	skipChainLabels := map[string]bool{}
	for s := range skipLabels {
		skipChainLabels[auxNormalizeChainLabel(s)] = true
	}
	skipBackend := auxFailedBackendSkip(failedProvider, "", failedBaseURL, scope)
	var tried []string
	for _, entry := range auxProviderChainFn() {
		candidateBaseURL := auxCustomHealthBaseURL(ctx, entry.label, "")
		if (failedBaseURL == "" && skipChainLabels[entry.label]) || skipBackend(entry.label, "", candidateBaseURL) {
			continue
		}
		if auxIsProviderUnhealthy(ctx, entry.label, candidateBaseURL) {
			auxLogSkipUnhealthy(ctx, entry.label, task, candidateBaseURL)
			tried = append(tried, entry.label+" (unhealthy)")
			continue
		}
		client, model, err := entry.try(ctx)
		if err != nil {
			return nil, "", "", err
		}
		if client != nil {
			slog.Info(fmt.Sprintf("Auxiliary %s: %s on %s — falling back to %s (%s)", auxTaskOrCall(task), reason, failedProvider, entry.label, auxOrDefault(model, "default")))
			return client, model, entry.label, nil
		}
		tried = append(tried, entry.label)
	}
	slog.Warn(fmt.Sprintf("Auxiliary %s: %s on %s and no fallback available (tried: %s)", auxTaskOrCall(task), reason, failedProvider, strings.Join(tried, ", ")))
	return nil, "", "", nil
}

// Candidate skip predicate for a failed route, model scoped with a failed model and credential wide without
func auxFailedBackendSkip(failedProvider, failedModel, failedBaseURL string, scope FailureScope) func(provider, model, baseURL string) bool {
	skipModel := strings.ToLower(strings.TrimSpace(failedModel))
	failed := BuildBackendIdentity(failedProvider, skipModel, failedBaseURL)
	if scope == "" {
		scope = ScopeCredential
		if skipModel != "" {
			scope = ScopeModel
		}
	}
	return func(provider, model, baseURL string) bool {
		return ShouldSkipCandidate(BuildBackendIdentity(provider, model, baseURL), failed, scope)
	}
}

// Main agent provider and model as the last resort after the configured chain is exhausted
func auxTryMainAgentModelFallback(ctx context.Context, failedProvider, task, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error) {
	mainProvider := strings.TrimSpace(auxReadMainProvider(ctx))
	mainModel := strings.TrimSpace(auxReadMainModel(ctx))
	if strings.ToLower(mainProvider) == "moa" {
		aggProvider, aggModel := auxResolveMoaAggregator(ctx, mainModel)
		if aggProvider == "" || aggModel == "" {
			return nil, "", "", nil
		}
		mainProvider, mainModel = aggProvider, aggModel
	}
	if mainProvider == "" || mainModel == "" || strings.ToLower(mainProvider) == "auto" {
		return nil, "", "", nil
	}
	if task == "vision" && (auxProvidersWithoutVision[mainProvider] || !auxMainModelSupportsVision(ctx, mainProvider, mainModel)) {
		slog.Info(fmt.Sprintf("Auxiliary vision: %s on %s — main agent provider %s accepts no image input, not falling back", reason, failedProvider, mainProvider))
		return nil, "", "", nil
	}
	mainBaseURL := auxCustomHealthBaseURL(ctx, mainProvider, "")
	if auxFailedBackendSkip(failedProvider, failedModel, failedBaseURL, scope)(mainProvider, mainModel, mainBaseURL) {
		return nil, "", "", nil
	}
	if auxIsProviderUnhealthy(ctx, mainProvider, mainBaseURL) {
		auxLogSkipUnhealthy(ctx, mainProvider, task, mainBaseURL)
		return nil, "", "", nil
	}
	client, resolvedModel, err := auxResolveProviderClientFn(ctx, mainProvider, mainModel, ProviderClientOptions{})
	if err != nil || client == nil {
		return nil, "", "", nil
	}
	label := "main-agent(" + mainProvider + ")"
	model := resolvedModel
	if model == "" {
		model = mainModel
	}
	slog.Info(fmt.Sprintf("Auxiliary %s: %s on %s — falling back to main agent model %s (%s)", auxTaskOrCall(task), reason, failedProvider, label, model))
	return client, model, label, nil
}

// Context floor of an auxiliary task, only compression has one
func auxTaskMinimumContextLength(task string) (int, bool) {
	if task == "compression" {
		return MinimumContextLength, true
	}
	return 0, false
}

// Chain entry resolver and context window lookup of the configured fallback chain, swapped by tests
var (
	auxResolveFallbackEntryFn = auxResolveFallbackEntry
	auxModelContextLengthFn   = GetModelContextLength
)

// Context window of a fallback candidate, false when unknown or the lookup failed
func auxCandidateContextWindow(ctx context.Context, provider, model, baseURL, apiKey string) (int, bool) {
	if model == "" {
		return 0, false
	}
	var window int
	if err := auxRecoverRaise(func() {
		window = auxModelContextLengthFn(ctx, ContextLengthRequest{Model: model, BaseURL: baseURL, APIKey: apiKey, Provider: provider})
	}); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary fallback: could not resolve context window for %s/%s: %v", provider, model, err))
		return 0, false
	}
	return window, window > 0
}

// Tried note of a candidate whose context window is under the task floor, empty when it fits or is unknown
func auxContextTooSmall(ctx context.Context, entry map[string]any, provider, model string, minCtx int, hasMin bool, task, label string, nameModel bool) string {
	if !hasMin {
		return ""
	}
	fbCtx, known := auxCandidateContextWindow(ctx, provider, model, auxStrOr(entry["base_url"]), auxFallbackEntryAPIKey(ctx, entry))
	if !known || fbCtx >= minCtx {
		return ""
	}
	if nameModel {
		slog.Info(fmt.Sprintf("Auxiliary %s: skipping %s (%s context=%d < min=%d), continuing chain", task, label, model, fbCtx, minCtx))
	} else {
		slog.Info(fmt.Sprintf("Auxiliary %s: skipping %s (context=%d < min=%d), continuing chain", auxTaskOrCall(task), label, fbCtx, minCtx))
	}
	return fmt.Sprintf("%s (context too small: %d<%d)", label, fbCtx, minCtx)
}

// First usable entry of the task fallback chain, skipping the failed backend, unhealthy and too small candidates
func auxTryConfiguredFallbackChain(ctx context.Context, task, failedProvider, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error) {
	if task == "" {
		return nil, "", "", nil
	}
	cfg, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil {
		return nil, "", "", err
	}
	chain, ok := cfg["fallback_chain"].([]any)
	if !ok || len(chain) == 0 {
		return nil, "", "", nil
	}
	skip := auxFailedBackendSkip(failedProvider, failedModel, failedBaseURL, scope)
	var tried []string
	minCtx, hasMin := auxTaskMinimumContextLength(task)
	for i, raw := range chain {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fbProvider := strings.TrimSpace(auxStrGet(entry, "provider"))
		if fbProvider == "" {
			continue
		}
		fbModelRaw := strings.TrimSpace(auxStrGet(entry, "model"))
		fbBaseURL := auxCustomHealthBaseURL(ctx, fbProvider, auxStrOr(entry["base_url"]))
		if skip(fbProvider, fbModelRaw, fbBaseURL) {
			continue
		}
		label := fmt.Sprintf("fallback_chain[%d](%s)", i, fbProvider)
		if auxIsProviderUnhealthy(ctx, fbProvider, fbBaseURL) {
			auxLogSkipUnhealthy(ctx, fbProvider, task, fbBaseURL)
			tried = append(tried, label+" (unhealthy)")
			continue
		}
		fbClient, resolvedModel, err := auxResolveFallbackEntryFn(ctx, entry)
		if err != nil {
			fbClient, resolvedModel = nil, ""
		}
		if fbClient != nil {
			if resolvedModel != "" {
				if tooSmall := auxContextTooSmall(ctx, entry, fbProvider, resolvedModel, minCtx, hasMin, task, label, true); tooSmall != "" {
					tried = append(tried, tooSmall)
					continue
				}
			}
			model := resolvedModel
			if model == "" {
				model = fbModelRaw
			}
			slog.Info(fmt.Sprintf("Auxiliary %s: %s on %s — configured fallback to %s (%s)", task, reason, failedProvider, label, auxOrDefault(model, "default")))
			return fbClient, model, label, nil
		}
		tried = append(tried, label)
	}
	if len(tried) > 0 {
		slog.Debug(fmt.Sprintf("Auxiliary %s: configured fallback_chain exhausted (tried: %s)", task, strings.Join(tried, ", ")))
	}
	return nil, "", "", nil
}

// Task fallback chain for an explicit provider that cannot build a client, never the main model
func auxTryConfiguredFallbackForUnavailableClient(ctx context.Context, task, failedProvider string) (AuxClient, string, string, error) {
	explicit := strings.ToLower(strings.TrimSpace(failedProvider))
	if task == "" || explicit == "" || explicit == "auto" {
		return nil, "", "", nil
	}
	return auxTryConfiguredFallbackChainFn(ctx, task, explicit, "provider unavailable", "", "", "")
}

// Task fallback chain client, model and label for an explicit provider that cannot build a client
func TryConfiguredFallbackForUnavailableClient(ctx context.Context, task, failedProvider string) (AuxClient, string, string, error) {
	return auxTryConfiguredFallbackForUnavailableClient(ctx, task, failedProvider)
}

// Inline or env backed key of a fallback entry through the secret scope aware resolver
func auxFallbackEntryAPIKey(ctx context.Context, entry map[string]any) string {
	h := home.Dir(ctx)
	return newResolver(ctx, config.Tree(LoadConfigSafe(ctx, h)), config.NewSecrets(h), h, nil).ResolveEntryAPIKey(entry)
}

// Inline or env backed key of a fallback entry through the secret scope aware resolver
func AuxFallbackEntryAPIKey(ctx context.Context, entry map[string]any) string {
	return auxFallbackEntryAPIKey(ctx, entry)
}

// Client and model of one fallback entry through the provider router, its destination attached
func auxResolveFallbackEntry(ctx context.Context, entry map[string]any) (AuxClient, string, error) {
	provider := strings.TrimSpace(auxStrOr(entry["provider"]))
	model := strings.TrimSpace(auxStrOr(entry["model"]))
	if provider == "" || model == "" {
		return nil, "", nil
	}
	apiMode := auxStrOr(entry["api_mode"])
	if apiMode == "" {
		apiMode = auxStrOr(entry["transport"])
	}
	var key any
	if k := auxFallbackEntryAPIKey(ctx, entry); k != "" {
		key = k
	}
	client, resolvedModel, err := auxResolveProviderClientFn(ctx, provider, model, ProviderClientOptions{
		ExplicitBaseURL: strings.TrimSpace(auxStrOr(entry["base_url"])), ExplicitAPIKey: key, APIMode: strings.TrimSpace(apiMode)})
	if err != nil {
		return nil, "", err
	}
	if client != nil {
		if holder, ok := client.(auxFallbackDestHolder); ok {
			if dest, derr := auxFallbackDestinationFromEntry(ctx, entry, client, resolvedModel); derr == nil {
				holder.auxSetFallbackDest(dest)
			}
		}
	}
	return client, resolvedModel, nil
}

// Main agent fallback_providers chain for an auto task, in the main agent's order
func auxTryMainFallbackChain(ctx context.Context, task, failedProvider, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error) {
	var chain []map[string]any
	if err := auxRecoverRaise(func() {
		cfg, err := auxLoadConfigReadonlyFn(ctx)
		if err != nil {
			panic(err)
		}
		chain = GetFallbackChain(map[string]any(cfg))
	}); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary %s: could not load main fallback chain: %v", auxTaskOrCall(task), err))
		return nil, "", "", nil
	}
	if len(chain) == 0 {
		return nil, "", "", nil
	}
	skip := auxFailedBackendSkip(failedProvider, failedModel, failedBaseURL, scope)
	var tried []string
	minCtx, hasMin := auxTaskMinimumContextLength(task)
	for i, entry := range chain {
		if entry == nil {
			continue
		}
		fbProvider := strings.TrimSpace(auxStrOr(entry["provider"]))
		fbModel := strings.TrimSpace(auxStrOr(entry["model"]))
		if fbProvider == "" || fbModel == "" {
			continue
		}
		fbNorm := strings.ToLower(fbProvider)
		label := fmt.Sprintf("fallback_providers[%d](%s)", i, fbProvider)
		fbBaseURL := auxCustomHealthBaseURL(ctx, fbProvider, auxStrOr(entry["base_url"]))
		if fbNorm == "auto" || skip(fbProvider, fbModel, fbBaseURL) {
			tried = append(tried, label+" (skipped)")
			continue
		}
		if auxIsProviderUnhealthy(ctx, fbNorm, fbBaseURL) {
			auxLogSkipUnhealthy(ctx, fbNorm, task, fbBaseURL)
			tried = append(tried, label+" (unhealthy)")
			continue
		}
		fbClient, resolvedModel, err := auxResolveFallbackEntryFn(ctx, entry)
		if err != nil {
			slog.Debug(fmt.Sprintf("Auxiliary %s: main fallback %s failed to resolve: %v", auxTaskOrCall(task), label, err))
			fbClient, resolvedModel = nil, ""
		}
		if fbClient != nil {
			model := resolvedModel
			if model == "" {
				model = fbModel
			}
			if tooSmall := auxContextTooSmall(ctx, entry, fbProvider, model, minCtx, hasMin, task, label, false); tooSmall != "" {
				tried = append(tried, tooSmall)
				continue
			}
			failed := failedProvider
			if failed == "" {
				failed = "auto"
			}
			slog.Info(fmt.Sprintf("Auxiliary %s: %s on %s — main fallback chain to %s (%s)", auxTaskOrCall(task), reason, failed, label, model))
			return fbClient, model, fbProvider, nil
		}
		tried = append(tried, label)
	}
	if len(tried) > 0 {
		slog.Debug(fmt.Sprintf("Auxiliary %s: main fallback chain exhausted (tried: %s)", auxTaskOrCall(task), strings.Join(tried, ", ")))
	}
	return nil, "", "", nil
}

// Warns once when OPENAI_BASE_URL is set while config names a non custom provider
func auxWarnStaleOpenAIBaseURL(ctx context.Context, runtimeProvider string) {
	if auxStaleBaseURLWarned.Load() {
		return
	}
	envBase := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	cfgProvider := runtimeProvider
	if cfgProvider == "" {
		cfgProvider = auxReadMainProvider(ctx)
	}
	if envBase != "" && cfgProvider != "" && cfgProvider != "custom" && !strings.HasPrefix(cfgProvider, "custom:") {
		slog.Warn(fmt.Sprintf("OPENAI_BASE_URL is set (%s) but model.provider is '%s'. "+
			"Auxiliary clients may route to the wrong endpoint. "+
			"Run: icebath model to reconfigure, or remove "+
			"OPENAI_BASE_URL from ~/.icebath/.env", envBase, cfgProvider))
		auxStaleBaseURLWarned.Store(true)
	}
}

type auxMainTarget struct {
	Provider, Model, BaseURL string
	APIKey                   any
	APIMode                  string
}

// Main runtime target after the fast model opt in and the MoA aggregator substitution
func auxMainRouteTarget(ctx context.Context, runtime map[string]any, task string) (auxMainTarget, error) {
	t := auxMainTarget{Provider: auxStrOr(runtime["provider"]), Model: auxStrOr(runtime["model"]),
		BaseURL: auxStrOr(runtime["base_url"]), APIKey: runtime["api_key"], APIMode: auxStrOr(runtime["api_mode"])}
	if t.Provider == "" {
		t.Provider = auxReadMainProvider(ctx)
	}
	if t.Model == "" {
		t.Model = auxReadMainModel(ctx)
	}
	if t.APIKey == nil {
		t.APIKey = ""
	}
	prefersFast, err := auxTaskPrefersFastModel(ctx, task)
	if err != nil {
		return t, err
	}
	if prefersFast && t.Provider != "" && t.Provider != "auto" {
		if fast := auxAuxModelForProvider(ctx, t.Provider, true); fast != "" && fast != t.Model {
			slog.Debug(fmt.Sprintf("Auxiliary task %s: preferring fast model %s over main model %s", task, fast, t.Model))
			t.Model = fast
		}
	}
	if t.Provider == "moa" {
		if aggProvider, aggModel := auxResolveMoaAggregator(ctx, t.Model); aggProvider != "" && aggModel != "" {
			t.Provider, t.Model = aggProvider, aggModel
			t.BaseURL, t.APIKey, t.APIMode = "", "", ""
		}
	}
	return t, nil
}

// Client, model and provider of the main provider route, nil client when the route is unusable
func auxTryMainProviderRoute(ctx context.Context, t auxMainTarget) (AuxClient, string, string, error) {
	if t.Provider == "" || t.Model == "" || t.Provider == "auto" {
		return nil, "", "", nil
	}
	resolvedProvider := t.Provider
	explicitBaseURL := t.BaseURL
	healthBaseURL := auxCustomHealthBaseURL(ctx, t.Provider, explicitBaseURL)
	var explicitAPIKey any
	switch {
	case t.BaseURL != "" && t.Provider == "custom":
		if auxKeyTruthy(t.APIKey) {
			explicitAPIKey = t.APIKey
		}
	case strings.HasPrefix(t.Provider, "custom:"):
		hasNamedEntry := auxGetNamedCustomProviderFn(ctx, t.Provider) != nil
		switch {
		case hasNamedEntry:
			explicitBaseURL = ""
		case t.BaseURL != "":
			resolvedProvider = "custom"
			if auxKeyTruthy(t.APIKey) {
				explicitAPIKey = t.APIKey
			}
		case auxKeyTruthy(t.APIKey):
			explicitAPIKey = t.APIKey
		}
	case auxKeyTruthy(t.APIKey):
		explicitAPIKey = t.APIKey
	}
	if label := auxNormalizeChainLabel(resolvedProvider); label != "" && auxIsProviderUnhealthy(ctx, label, healthBaseURL) {
		auxLogSkipUnhealthy(ctx, label, "", healthBaseURL)
		return nil, "", "", nil
	}
	client, resolved, err := auxResolveProviderClientFn(ctx, resolvedProvider, t.Model, ProviderClientOptions{
		ExplicitBaseURL: explicitBaseURL, ExplicitAPIKey: explicitAPIKey, APIMode: t.APIMode})
	if err != nil || client == nil {
		return nil, "", "", err
	}
	if resolved == "" {
		resolved = t.Model
	}
	slog.Info(fmt.Sprintf("Auxiliary auto-detect: using main provider %s (%s)", t.Provider, resolved))
	return client, resolved, resolvedProvider, nil
}

// Discovery chain only for installs with no selected main provider, warning when a selection forbids guessing
func auxDiscoveryChainAllowed(mainProvider, task string) bool {
	if p := strings.ToLower(strings.TrimSpace(mainProvider)); p == "" || p == "auto" {
		return true
	}
	slog.Warn(fmt.Sprintf("Auxiliary %s: main provider %s is unavailable and no fallback_chain / fallback_providers is "+
		"configured — refusing to guess another logged-in provider. Re-authenticate (`icebath model`) "+
		"or declare a fallback.", auxTaskOrCall(task), mainProvider))
	return false
}

// Built in aggregator chain skipping unhealthy providers, nil client when none is available
func auxTryDiscoveryChain(ctx context.Context) (AuxClient, string, string, error) {
	var tried []string
	for _, entry := range auxProviderChainFn() {
		candidateBaseURL := auxCustomHealthBaseURL(ctx, entry.label, "")
		if auxIsProviderUnhealthy(ctx, entry.label, candidateBaseURL) {
			auxLogSkipUnhealthy(ctx, entry.label, "", candidateBaseURL)
			tried = append(tried, entry.label+" (unhealthy)")
			continue
		}
		client, model, err := entry.try(ctx)
		if err != nil {
			return nil, "", "", err
		}
		if client != nil {
			if len(tried) > 0 {
				slog.Info(fmt.Sprintf("Auxiliary auto-detect: using %s (%s) — skipped: %s", entry.label, auxOrDefault(model, "default"), strings.Join(tried, ", ")))
			} else {
				slog.Info(fmt.Sprintf("Auxiliary auto-detect: using %s (%s)", entry.label, auxOrDefault(model, "default")))
			}
			return client, model, entry.label, nil
		}
		tried = append(tried, entry.label)
	}
	slog.Warn(fmt.Sprintf("Auxiliary auto-detect: no provider available (tried: %s). "+
		"Compression, summarization, and memory flush will not work. "+
		"Set OPENROUTER_API_KEY or configure a local model in config.yaml.", strings.Join(tried, ", ")))
	return nil, "", "", nil
}

// Main provider route, then the configured fallback policy, then discovery when no provider was selected
func auxResolveAutoRoute(ctx context.Context, mainRuntime map[string]any, task string) (AuxClient, string, string, error) {
	runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
	runtimeProvider, _ := runtime["provider"].(string)
	auxWarnStaleOpenAIBaseURL(ctx, runtimeProvider)
	target, err := auxMainRouteTarget(ctx, runtime, task)
	if err != nil {
		return nil, "", "", err
	}
	client, model, provider, err := auxTryMainProviderRouteFn(ctx, target)
	if err != nil || client != nil {
		return client, model, provider, err
	}
	failed := target.Provider
	if failed == "" {
		failed = "auto"
	}
	if task != "" {
		fbClient, fbModel, fbLabel, err := auxTryConfiguredFallbackChainFn(ctx, task, failed, "main provider unavailable", "", "", "")
		if err != nil {
			return nil, "", "", err
		}
		if fbClient != nil {
			return fbClient, fbModel, auxFallbackProviderFromLabel(fbLabel), nil
		}
	}
	fbClient, fbModel, fbLabel, err := auxTryMainFallbackChainFn(ctx, task, failed, "main provider unavailable", "", "", "")
	if err != nil || fbClient != nil {
		return fbClient, fbModel, fbLabel, err
	}
	if !auxDiscoveryChainAllowed(target.Provider, task) {
		return nil, "", "", nil
	}
	return auxTryDiscoveryChain(ctx)
}

// Fallback walk seams that tests swap the way hermes tests patch the module functions
var (
	auxTryMainProviderRouteFn       func(ctx context.Context, t auxMainTarget) (AuxClient, string, string, error)
	auxTryConfiguredFallbackChainFn func(ctx context.Context, task, failedProvider, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error)
	auxTryMainFallbackChainFn       func(ctx context.Context, task, failedProvider, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error)
	auxTryPaymentFallbackFn         func(ctx context.Context, failedProvider, task, reason, failedBaseURL string, scope FailureScope, mainRuntime map[string]any) (AuxClient, string, string, error)
	auxTryMainAgentModelFallbackFn  func(ctx context.Context, failedProvider, task, reason, failedModel, failedBaseURL string, scope FailureScope) (AuxClient, string, string, error)
	auxProviderChainFn              func() []auxProviderChainEntry
)

func init() {
	auxTryMainProviderRouteFn = auxTryMainProviderRoute
	auxTryConfiguredFallbackChainFn = auxTryConfiguredFallbackChain
	auxTryMainFallbackChainFn = auxTryMainFallbackChain
	auxTryPaymentFallbackFn = auxTryPaymentFallback
	auxTryMainAgentModelFallbackFn = auxTryMainAgentModelFallback
	auxProviderChainFn = auxGetProviderChain
}

// Auto route seam the cached auto route test swaps
var auxResolveAutoRouteFn = auxResolveAutoRoute
