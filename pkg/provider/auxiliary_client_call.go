package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Keyword arguments of one call_llm or async_call_llm request
type auxCallRequest struct {
	Task, Provider, Model, BaseURL string
	APIKey                         any
	MainRuntime                    map[string]any
	Messages                       any
	Temperature                    *float64
	MaxTokens                      any
	Tools                          any
	Timeout                        *float64
	TimeoutText                    string
	ExtraBody                      map[string]any
	ReasoningConfig                map[string]any
	ExtraHeaders                   map[string]string
	APIMode                        string
	Stream                         bool
	StreamOptions                  map[string]any
	RouteInfo                      map[string]string
	LatencyInfo                    map[string]any
}

var auxAsyncCallKeys = map[string]bool{"task": true, "provider": true, "model": true, "base_url": true, "api_key": true,
	"main_runtime": true, "messages": true, "temperature": true, "max_tokens": true, "tools": true, "timeout": true,
	"extra_body": true, "reasoning_config": true, "route_info": true}

var auxSyncOnlyCallKeys = map[string]bool{"extra_headers": true, "api_mode": true, "stream": true, "stream_options": true, "latency_info": true}

// Float pointer of a numeric kwarg, nil for None
func auxFloatKwarg(name string, v any) (*float64, error) {
	switch n := v.(type) {
	case nil:
		return nil, nil
	case float64:
		return &n, nil
	case float32:
		f := float64(n)
		return &f, nil
	case int:
		f := float64(n)
		return &f, nil
	case int64:
		f := float64(n)
		return &f, nil
	}
	return nil, fmt.Errorf("%s must be a number, got %s", name, auxTypeName(v))
}

// Call request of keyword arguments, a TypeError shaped error for unknown or missing ones
func auxParseCallKwargs(fn string, kwargs map[string]any, sync bool) (auxCallRequest, error) {
	var req auxCallRequest
	for k := range kwargs {
		if !auxAsyncCallKeys[k] && !(sync && auxSyncOnlyCallKeys[k]) {
			return req, fmt.Errorf("%s() got an unexpected keyword argument '%s'", fn, k)
		}
	}
	messages, ok := kwargs["messages"]
	if !ok {
		return req, fmt.Errorf("%s() missing 1 required keyword-only argument: 'messages'", fn)
	}
	req.Messages = messages
	req.Task, _ = kwargs["task"].(string)
	req.Provider, _ = kwargs["provider"].(string)
	req.Model, _ = kwargs["model"].(string)
	req.BaseURL, _ = kwargs["base_url"].(string)
	req.APIKey = kwargs["api_key"]
	req.MainRuntime, _ = kwargs["main_runtime"].(map[string]any)
	req.MaxTokens = kwargs["max_tokens"]
	if tools, ok := kwargs["tools"]; ok && tools != nil {
		req.Tools = tools
	}
	req.ExtraBody, _ = kwargs["extra_body"].(map[string]any)
	req.ReasoningConfig, _ = kwargs["reasoning_config"].(map[string]any)
	req.RouteInfo, _ = kwargs["route_info"].(map[string]string)
	req.APIMode, _ = kwargs["api_mode"].(string)
	req.Stream, _ = kwargs["stream"].(bool)
	req.StreamOptions, _ = kwargs["stream_options"].(map[string]any)
	req.LatencyInfo, _ = kwargs["latency_info"].(map[string]any)
	if headers, ok := kwargs["extra_headers"].(map[string]string); ok {
		req.ExtraHeaders = headers
	} else if headers, ok := kwargs["extra_headers"].(map[string]any); ok {
		req.ExtraHeaders = make(map[string]string, len(headers))
		for k, v := range headers {
			req.ExtraHeaders[k] = pyrepr.Str(v)
		}
	}
	var err error
	if req.Temperature, err = auxFloatKwarg("temperature", kwargs["temperature"]); err != nil {
		return req, err
	}
	if req.Timeout, err = auxFloatKwarg("timeout", kwargs["timeout"]); err != nil {
		return req, err
	}
	if kwargs["timeout"] != nil {
		req.TimeoutText = pyrepr.Str(kwargs["timeout"])
	}
	return req, nil
}

// Task argument of the config readers, nil standing for a request that names no task
func auxTaskArg(task string) any {
	if task == "" {
		return nil
	}
	return task
}

// Python rendering of an optional task in a user facing message
func auxTaskText(task string) string {
	if task == "" {
		return "None"
	}
	return task
}

// Client for one call from the vision chain or the cached text client with the fallback and auto chain rescue
func auxResolveCallClient(ctx context.Context, req auxCallRequest, route auxTaskRoute, asyncMode bool) (auxResolvedAuxRoute, error) {
	task := req.Task
	resolvedProvider := route.Provider
	effectiveProvider := resolvedProvider
	var client AuxClient
	var finalModel string
	var err error
	if task == "vision" {
		provider := req.Provider
		if resolvedProvider != "auto" {
			provider = resolvedProvider
		}
		model := auxOrDefault(route.Model, req.Model)
		baseURL := auxOrDefault(route.BaseURL, req.BaseURL)
		apiKey := route.APIKey
		if !auxKeyTruthy(apiKey) {
			apiKey = req.APIKey
		}
		effectiveProvider, client, finalModel, err = auxResolveVisionProviderClientFn(ctx, provider, model, baseURL, apiKey, asyncMode, req.MainRuntime)
		if err != nil {
			return auxResolvedAuxRoute{}, err
		}
		if client == nil && resolvedProvider != "auto" && route.BaseURL == "" {
			slog.Warn(fmt.Sprintf("Vision provider %s unavailable, falling back to auto vision backends", resolvedProvider))
			effectiveProvider, client, finalModel, err = auxResolveVisionProviderClientFn(ctx, "auto", route.Model, "", nil, asyncMode, req.MainRuntime)
			if err != nil {
				return auxResolvedAuxRoute{}, err
			}
		}
		if client != nil {
			resolvedProvider = auxOrDefault(effectiveProvider, resolvedProvider)
		}
	} else {
		client, finalModel, err = auxGetCachedClientFn(ctx, auxCachedClientRequest{Provider: resolvedProvider, Model: route.Model, AsyncMode: asyncMode,
			BaseURL: route.BaseURL, APIKey: route.APIKey, APIMode: route.APIMode, MainRuntime: req.MainRuntime, Task: task})
		if err != nil {
			return auxResolvedAuxRoute{}, err
		}
		effectiveProvider = auxEffectiveProviderForClient(client, resolvedProvider)
		if client == nil {
			explicit := strings.ToLower(strings.TrimSpace(resolvedProvider))
			if explicit != "" && explicit != "auto" && explicit != "openrouter" && explicit != "custom" {
				fbClient, fbModel, fbLabel, err := auxTryConfiguredFallbackForUnavailableClient(ctx, task, explicit)
				if err != nil {
					return auxResolvedAuxRoute{}, err
				}
				if fbClient == nil {
					if explicit == "nous" {
						if detail, ok := NousCredentialFailureDetail(); ok && detail != "" {
							return auxResolvedAuxRoute{}, &AuxiliaryClientUnavailable{Message: detail}
						}
					}
					return auxResolvedAuxRoute{}, &AuxiliaryClientUnavailable{Message: MissingProviderCredentialsMessage(ctx, explicit)}
				}
				client, finalModel = fbClient, fbModel
				if asyncMode {
					client, finalModel = auxToAsyncClientFn(ctx, fbClient, fbModel, task == "vision")
				}
				resolvedProvider = auxOrDefault(fbLabel, resolvedProvider)
				effectiveProvider = resolvedProvider
			}
			if client == nil && route.BaseURL == "" {
				slog.Info(fmt.Sprintf("Auxiliary %s: provider %s unavailable, trying auto-detection chain", auxTaskOrCall(task), resolvedProvider))
				client, finalModel, err = auxGetCachedClientFn(ctx, auxCachedClientRequest{Provider: "auto", AsyncMode: asyncMode, MainRuntime: req.MainRuntime, Task: task})
				if err != nil {
					return auxResolvedAuxRoute{}, err
				}
				effectiveProvider = auxEffectiveProviderForClient(client, "auto")
			}
		}
	}
	if client == nil {
		return auxResolvedAuxRoute{}, &AuxiliaryClientUnavailable{Message: fmt.Sprintf("No LLM provider configured for task=%s provider=%s. Run: icebath setup",
			auxTaskText(task), resolvedProvider)}
	}
	return auxResolvedAuxRoute{Client: client, FinalModel: finalModel, ResolvedProvider: resolvedProvider, EffectiveProvider: effectiveProvider}, nil
}

type auxPreparedAuxRequest struct {
	Client               AuxClient
	FinalModel           string
	Kwargs               map[string]any
	ResolvedProvider     string
	RequestProvider      string
	ResolvedModel        string
	ResolvedBaseURL      string
	ResolvedAPIKey       any
	ResolvedAPIMode      string
	EffectiveTimeout     float64
	EffectiveTimeoutText string
	EffectiveExtraBody   map[string]any
	BaseInfo             string
}

// Route and client resolved, the route published and the request kwargs built for one call
func auxPrepareAuxRequest(ctx context.Context, req auxCallRequest, asyncMode bool) (auxPreparedAuxRequest, error) {
	task := req.Task
	route := auxResolveTaskProviderModelFn(ctx, auxTaskArg(task), req.Provider, req.Model, req.BaseURL, req.APIKey)
	if req.APIMode != "" {
		route.APIMode = req.APIMode
	}
	extraBody, err := auxGetTaskExtraBody(ctx, task)
	if err != nil {
		return auxPreparedAuxRequest{}, err
	}
	if extraBody == nil {
		extraBody = map[string]any{}
	}
	for k, v := range req.ExtraBody {
		extraBody[k] = v
	}
	resolved, err := auxResolveCallClient(ctx, req, route, asyncMode)
	if err != nil {
		return auxPreparedAuxRequest{}, err
	}
	client, finalModel := resolved.Client, resolved.FinalModel
	effectiveTimeout := EffectiveAuxTimeout(ctx, task, req.Timeout)
	timeoutText := req.TimeoutText
	if req.Timeout == nil || timeoutText == "" {
		timeoutText = pyrepr.Str(effectiveTimeout)
	}
	var noProgressTimeout *float64
	switch client.(type) {
	case *CodexAuxiliaryClient, *AsyncCodexAuxiliaryClient:
		v := auxGetTaskNoProgressTimeoutFn(ctx, task)
		noProgressTimeout = &v
	}
	requestProvider := auxOrDefault(resolved.EffectiveProvider, resolved.ResolvedProvider)
	if !asyncMode {
		compressionConfig := map[string]any{}
		if task == "compression" {
			if compressionConfig, err = auxGetAuxiliaryTaskConfig(ctx, task); err != nil {
				return auxPreparedAuxRequest{}, err
			}
		}
		if _, extraBody, err = auxCompressionFastLaneControls(ctx, task, requestProvider, finalModel, req.Provider, req.Model,
			compressionConfig, compressionConfig, req.MaxTokens, extraBody); err != nil {
			return auxPreparedAuxRequest{}, err
		}
	}
	auxSetRelayAuxiliaryRoute(ctx, requestProvider, finalModel, route.APIMode)
	auxRecordRouteInfo(req.RouteInfo, auxFallbackProviderFromLabel(requestProvider), finalModel)
	baseInfo := client.BaseURL()
	if !asyncMode && task != "" {
		at := ""
		if baseInfo != "" && !strings.Contains(baseInfo, "openrouter") {
			at = " at " + baseInfo
		}
		slog.Info(fmt.Sprintf("Auxiliary %s: using %s (%s)%s", task, auxOrDefault(requestProvider, "auto"), auxOrDefault(finalModel, "default"), at))
	}
	kwargs, err := auxBuildCallKwargs(ctx, requestProvider, finalModel, req.Messages, auxCallKwargsOptions{Temperature: req.Temperature,
		MaxTokens: req.MaxTokens, Tools: req.Tools, Timeout: effectiveTimeout, ExtraBody: extraBody, ReasoningConfig: req.ReasoningConfig,
		BaseURL: auxOrDefault(baseInfo, route.BaseURL), Task: task, NoProgressTimeout: noProgressTimeout})
	if err != nil {
		return auxPreparedAuxRequest{}, err
	}
	if len(req.ExtraHeaders) > 0 {
		headers := make(map[string]string, len(req.ExtraHeaders))
		for k, v := range req.ExtraHeaders {
			headers[k] = v
		}
		kwargs["extra_headers"] = headers
	}
	if auxIsAnthropicCompatEndpoint(requestProvider, client.BaseURL()) {
		kwargs["messages"] = auxConvertOpenAIImagesToAnthropic(kwargs["messages"])
	}
	return auxPreparedAuxRequest{Client: client, FinalModel: finalModel, Kwargs: kwargs, ResolvedProvider: resolved.ResolvedProvider,
		RequestProvider: requestProvider, ResolvedModel: route.Model, ResolvedBaseURL: route.BaseURL, ResolvedAPIKey: route.APIKey,
		ResolvedAPIMode: route.APIMode, EffectiveTimeout: effectiveTimeout, EffectiveTimeoutText: timeoutText, EffectiveExtraBody: extraBody, BaseInfo: baseInfo}, nil
}

type auxLadderStep struct {
	Kind string
	Args []any
}

type auxLadderYield func(step auxLadderStep) (any, error)

// Vision resolution, response validation and async wrapping, swapped by tests the way hermes tests patch them
var (
	auxResolveVisionProviderClientFn = ResolveVisionProviderClient
	auxValidateLLMResponseFn         = auxValidateLLMResponse
	auxToAsyncClientFn               = auxToAsyncClient
)

// Credential, refresh and provider fallback collaborators of the recovery ladder, swapped by tests
var (
	auxLadderProviderFallbackFn      = auxLadderProviderFallback
	auxRefreshNousAuxiliaryClientFn  = auxRefreshNousAuxiliaryClient
	auxNousFreshPaidAccessFn         = auxNousPortalAccountHasFreshPaidAccess
	auxAuthRefreshProviderForRouteFn = auxAuthRefreshProviderForRoute
	auxRefreshProviderCredentialsFn  = auxRefreshProviderCredentials
	auxEvictCachedClientsFn          = auxEvictCachedClients
	auxRecoverablePoolProviderFn     = auxRecoverablePoolProvider
)

type auxFallbackReason struct {
	Predicate func(error) bool
	Label     string
}

var auxFallbackReasons []auxFallbackReason

func init() {
	auxFallbackReasons = []auxFallbackReason{
		{auxIsAuthError, "auth error"}, {auxIsPaymentError, "payment error"},
		{auxIsRateLimitError, "rate limit"}, {auxIsModelIncompatibleError, "model incompatible with route"},
		{auxIsInvalidAuxResponseError, "invalid provider response"},
		{auxIsStatuslessStructuredProviderError, "structured provider error"},
		{auxIsTimeoutError, "request timed out"}, {auxIsConnectionError, "connection error"},
	}
}

// One rung performing the step, its error accepted for the next rung or raised
func auxRung(yield auxLadderYield, step auxLadderStep, accept func(error) bool) (any, error, error) {
	result, err := yield(step)
	if err != nil {
		if !accept(err) {
			return nil, nil, err
		}
		return nil, err, nil
	}
	return result, nil, nil
}

// Errors a parameter strip retry hands on to the later rungs
func auxParamRungAccepts(err error) bool {
	text := err.Error()
	return auxIsPaymentError(err) || auxIsConnectionError(err) || auxIsAuthError(err) || auxIsRateLimitError(err) ||
		auxIsStatuslessStructuredProviderError(err) || strings.Contains(text, "max_tokens") || strings.Contains(text, "unsupported_parameter") ||
		auxIsUnsupportedParameterError(err, "temperature") || auxIsReasoningFieldRejection(err) || auxIsReasoningRequiredRejection(err) ||
		auxIsStructuredOutputRejection(err) || auxIsModelIncompatibleError(err)
}

// Errors the credential rungs act on
func auxCredentialRungAccepts(err error) bool {
	return auxIsAuthError(err) || auxIsPaymentError(err) || auxIsRateLimitError(err)
}

type auxLadderRoute struct {
	Client           AuxClient
	Task             string
	Tag              string
	AsyncMode        bool
	BaseInfo         string
	ResolvedProvider string
	ResolvedModel    string
	ResolvedBaseURL  string
	ResolvedAPIKey   any
	ResolvedAPIMode  string
	FinalModel       string
	MainRuntime      map[string]any
	RouteInfo        map[string]string
	Timeout          float64
	TimeoutText      string
}

// Copy of kwargs without temperature, nil when it was not sent
func auxWithoutTemperature(kwargs map[string]any) map[string]any {
	if _, ok := kwargs["temperature"]; !ok {
		return nil
	}
	out := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		if k != "temperature" {
			out[k] = v
		}
	}
	return out
}

// Copy of kwargs without either output cap, never nil
func auxWithoutMaxTokens(kwargs map[string]any) map[string]any {
	out := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		if k != "max_tokens" && k != "max_completion_tokens" {
			out[k] = v
		}
	}
	return out
}

// Rejection of the output cap, including the ZAI vision code 1210 that never names max_tokens
func auxIsMaxTokensRejection(err error, client AuxClient) bool {
	text := err.Error()
	baseURL := ""
	if client != nil {
		baseURL = client.BaseURL()
	}
	zai := strings.Contains(text, "1210") && strings.Contains(baseURL, "bigmodel")
	return strings.Contains(text, "max_tokens") || strings.Contains(text, "unsupported_parameter") || auxIsUnsupportedParameterError(err, "max_tokens") || zai
}

type auxParameterRung struct {
	matches  func(error) bool
	strip    func(map[string]any) map[string]any
	message  string
	remember func(provider, baseURL string, kwargs map[string]any, err error) error
}

// Ordered parameter rungs whose strip returns nil when the field was not on the wire
func auxParameterRungs(client AuxClient, maxTokens any) []auxParameterRung {
	return []auxParameterRung{
		{func(err error) bool { return auxIsUnsupportedParameterError(err, "temperature") }, auxWithoutTemperature,
			"provider rejected temperature; retrying without it", RememberTemperatureRejection},
		{auxIsStructuredOutputRejection, auxWithoutStructuredOutputFormat,
			"provider rejected the structured-output format field; retrying without it " +
				"(schema enforcement degrades to prompt compliance)", RememberStructuredOutputRejection},
		{auxIsReasoningRequiredRejection, WithReasoningFloor, "provider requires reasoning; retrying at the floor effort", RememberReasoningFloor},
		{auxIsReasoningFieldRejection, auxWithoutReasoningFields, "provider rejected the reasoning field; retrying without it (route default applies)", nil},
		{func(err error) bool { return maxTokens != nil && auxIsMaxTokensRejection(err, client) }, auxWithoutMaxTokens,
			"provider rejected the output cap; retrying without it", nil},
	}
}

// Parameter rungs chained in the order the provider raises them, each field stripped at most once
func auxLadderParameterRungs(yield auxLadderYield, firstErr error, route auxLadderRoute, kwargs map[string]any, maxTokens any) (any, error, map[string]any, error) {
	rungs := auxParameterRungs(route.Client, maxTokens)
	for len(rungs) > 0 {
		hit := -1
		for i, r := range rungs {
			if r.matches(firstErr) && r.strip(kwargs) != nil {
				hit = i
				break
			}
		}
		if hit < 0 {
			break
		}
		r := rungs[hit]
		rungs = append(rungs[:hit:hit], rungs[hit+1:]...)
		retryKwargs := r.strip(kwargs)
		slog.Info(fmt.Sprintf("Auxiliary %s%s: %s: %v", auxTaskOrCall(route.Task), route.Tag, r.message, firstErr))
		rejection := firstErr
		resp, accepted, raised := auxRung(yield, auxLadderStep{Kind: "call", Args: []any{route.Client, retryKwargs}}, auxParamRungAccepts)
		if raised != nil {
			return nil, nil, nil, raised
		}
		if accepted == nil {
			if r.remember != nil {
				if err := r.remember(route.ResolvedProvider, auxOrDefault(route.BaseInfo, route.ResolvedBaseURL), kwargs, rejection); err != nil {
					return nil, nil, nil, err
				}
			}
			return resp, nil, retryKwargs, nil
		}
		firstErr = accepted
		kwargs = retryKwargs
	}
	return nil, firstErr, kwargs, nil
}

// Call step on a rebuilt Nous client after a credential event, nil when nothing refreshed
func auxRefreshedNousStep(ctx context.Context, route auxLadderRoute, kwargs map[string]any, message string) (*auxLadderStep, error) {
	client, model, err := auxRefreshNousAuxiliaryClientFn(ctx, auxNousRefreshRequest{CacheProvider: auxOrDefault(route.ResolvedProvider, "nous"),
		Model: route.FinalModel, LookupModel: route.ResolvedModel, LookupTask: route.Task, AsyncMode: route.AsyncMode, BaseURL: route.ResolvedBaseURL,
		APIKey: route.ResolvedAPIKey, APIMode: route.ResolvedAPIMode, MainRuntime: route.MainRuntime, IsVision: route.Task == "vision"})
	if err != nil || client == nil {
		return nil, err
	}
	slog.Info(fmt.Sprintf(message, auxTaskOrCall(route.Task), route.Tag))
	if model != "" && model != kwargs["model"] {
		kwargs["model"] = model
	}
	return &auxLadderStep{Kind: "call", Args: []any{client, kwargs}}, nil
}

// Nous stale model heal, paid account refresh and 401 refresh rungs
func auxLadderNousRungs(ctx context.Context, yield auxLadderYield, firstErr error, route auxLadderRoute, kwargs map[string]any, clientIsNous bool) (any, error, error) {
	task, tag := route.Task, route.Tag
	refreshAccepts := func(err error) bool { return auxCredentialRungAccepts(err) || auxIsConnectionError(err) }
	if auxIsModelNotFoundError(firstErr) && clientIsNous {
		stale, _ := kwargs["model"].(string)
		if healed := auxRefreshNousRecommendedModel(ctx, task == "vision", stale); healed != "" && healed != stale {
			slog.Warn(fmt.Sprintf("Auxiliary %s%s: model %s no longer in Nous catalog; retrying with refreshed recommendation %s",
				auxTaskOrCall(task), tag, pyrepr.Repr(kwargs["model"]), pyrepr.Repr(healed)))
			kwargs["model"] = healed
			resp, accepted, raised := auxRung(yield, auxLadderStep{Kind: "call", Args: []any{route.Client, kwargs}}, func(error) bool { return true })
			if raised != nil {
				return nil, nil, raised
			}
			if accepted == nil {
				return resp, nil, nil
			}
			firstErr = accepted
		}
	}
	if auxIsPaymentError(firstErr) && clientIsNous && auxNousFreshPaidAccessFn(ctx) {
		step, err := auxRefreshedNousStep(ctx, route, kwargs, "Auxiliary %s%s: refreshed Nous runtime credentials after paid account check, retrying")
		if err != nil {
			return nil, nil, err
		}
		if step != nil {
			resp, accepted, raised := auxRung(yield, *step, refreshAccepts)
			if raised != nil {
				return nil, nil, raised
			}
			if accepted == nil {
				return resp, nil, nil
			}
			firstErr = accepted
		}
	}
	if auxIsAuthError(firstErr) && clientIsNous {
		step, err := auxRefreshedNousStep(ctx, route, kwargs, "Auxiliary %s%s: refreshed Nous runtime credentials after 401, retrying")
		if err != nil {
			return nil, nil, err
		}
		if step != nil {
			resp, accepted, raised := auxRung(yield, *step, refreshAccepts)
			if raised != nil {
				return nil, nil, raised
			}
			if accepted == nil {
				return resp, nil, nil
			}
			firstErr = accepted
		}
	}
	return nil, firstErr, nil
}

// OAuth refresh with a same provider retry, then credential pool rotation
func auxLadderCredentialRungs(ctx context.Context, yield auxLadderYield, firstErr error, route auxLadderRoute, kwargs map[string]any, clientIsNous bool) (any, error, error) {
	client, task, tag, resolvedProvider := route.Client, route.Task, route.Tag, route.ResolvedProvider
	authRefreshProvider := auxAuthRefreshProviderForRouteFn(ctx, resolvedProvider, route.BaseInfo, auxEffectiveProviderForClient(client, ""))
	if auxIsAuthError(firstErr) && authRefreshProvider != "auto" && authRefreshProvider != "" && !clientIsNous {
		key := ""
		if authRefreshProvider == "anthropic" {
			key = auxClientAPIKeyText(client)
		}
		if auxRefreshProviderCredentialsFn(ctx, authRefreshProvider, key) {
			if authRefreshProvider != auxNormalizeAuxProvider(ctx, resolvedProvider) {
				auxEvictCachedClientsFn(ctx, resolvedProvider)
			}
			slog.Info(fmt.Sprintf("Auxiliary %s%s: refreshed %s credentials after auth error, retrying", auxTaskOrCall(task), tag, authRefreshProvider))
			step := auxLadderStep{Kind: "retry_same_provider", Args: []any{authRefreshProvider, auxOrDefault(route.ResolvedModel, route.FinalModel)}}
			resp, accepted, raised := auxRung(yield, step, func(err error) bool { return auxCredentialRungAccepts(err) || auxIsConnectionError(err) })
			if raised != nil {
				return nil, nil, raised
			}
			if accepted == nil {
				return resp, nil, nil
			}
			firstErr = accepted
		}
	}
	poolProvider, err := auxRecoverablePoolProviderFn(ctx, resolvedProvider, client, route.MainRuntime)
	if err != nil {
		return nil, nil, err
	}
	clientAPIKey := auxClientAPIKeyText(client)
	if poolProvider != "" && auxCredentialRungAccepts(firstErr) {
		recoveryErr := firstErr
		if auxIsRateLimitError(firstErr) && !auxIsPaymentError(firstErr) {
			resp, accepted, raised := auxRateLimitRetryRungs(yield, firstErr, client, kwargs, auxTaskOrCall(task)+tag)
			if raised != nil {
				return nil, nil, raised
			}
			if accepted == nil {
				return resp, nil, nil
			}
			recoveryErr = accepted
		}
		if auxRecoverProviderPool(ctx, poolProvider, recoveryErr, clientAPIKey) {
			slog.Info(fmt.Sprintf("Auxiliary %s%s: recovered %s via credential-pool rotation after %s", auxTaskOrCall(task), tag, poolProvider, auxTypeName(recoveryErr)))
			resp, retryErr := yield(auxLadderStep{Kind: "retry_same_provider", Args: []any{resolvedProvider, route.ResolvedModel}})
			if retryErr == nil {
				return resp, nil, nil
			}
			if !(auxIsPaymentError(retryErr) || auxIsAuthError(retryErr) || auxIsRateLimitError(retryErr)) {
				return nil, nil, retryErr
			}
			auxRecoverProviderPool(ctx, poolProvider, retryErr, "")
			firstErr = retryErr
		}
	}
	return nil, firstErr, nil
}

// Next candidate after a fallback entry was quarantined mid request, configured entries before discovery
func auxNextFallbackAfterQuarantine(ctx context.Context, task, resolvedProvider string, isAuto bool, route auxLadderRoute, failedModel string, scope FailureScope, taskChainOnly bool) (AuxClient, string, string, error) {
	const reason = "fallback candidate unavailable"
	failed := auxOrDefault(resolvedProvider, "auto")
	client, model, label, err := auxTryConfiguredFallbackChainFn(ctx, task, failed, reason, failedModel, route.BaseInfo, scope)
	if err != nil || taskChainOnly {
		return client, model, label, err
	}
	if client == nil && isAuto {
		if client, model, label, err = auxTryMainFallbackChainFn(ctx, task, failed, reason, failedModel, route.BaseInfo, scope); err != nil {
			return nil, "", "", err
		}
	}
	if client == nil {
		return auxTryPaymentFallbackFn(ctx, resolvedProvider, task, reason, route.BaseInfo, scope, route.MainRuntime)
	}
	return client, model, label, nil
}

// Last rung walking the configured chains, the main fallback and discovery or the main agent model
func auxLadderProviderFallback(ctx context.Context, yield auxLadderYield, firstErr error, route auxLadderRoute) (any, error) {
	task, tag, resolvedProvider := route.Task, route.Tag, route.ResolvedProvider
	isAuto := resolvedProvider == "auto" || resolvedProvider == ""
	reason := ""
	isCapacity := false
	for _, r := range auxFallbackReasons {
		if r.Predicate(firstErr) {
			if reason == "" {
				reason = r.Label
			}
			if r.Label != "auth error" {
				isCapacity = true
			}
		}
	}
	hasTaskChain := false
	if task != "" {
		cfg, err := auxGetAuxiliaryTaskConfig(ctx, task)
		if err != nil {
			return nil, err
		}
		chain, ok := cfg["fallback_chain"].([]any)
		hasTaskChain = ok && len(chain) > 0
	}
	explicitAuthWithTaskChain := reason == "auth error" && !isAuto && hasTaskChain
	if reason == "" || !(isAuto || isCapacity || explicitAuthWithTaskChain) {
		return nil, nil
	}
	if reason == "payment error" {
		pool, err := auxRecoverablePoolProvider(ctx, resolvedProvider, route.Client, route.MainRuntime)
		if err != nil {
			return nil, err
		}
		auxMarkProviderUnhealthy(ctx, auxOrDefault(pool, resolvedProvider), auxUnhealthyTTLSeconds, route.BaseInfo, auxUnhealthyPaymentReason, slog.LevelWarn)
	}
	if reason == "request timed out" {
		stalled := strings.Contains(firstErr.Error(), "Auxiliary chat stream")
		detail := fmt.Sprintf("timed out after %ss", auxOrDefault(route.TimeoutText, pyrepr.Str(route.Timeout)))
		knob := "timeout"
		if stalled {
			detail, knob = firstErr.Error(), "no_progress_timeout"
		}
		slog.Warn(fmt.Sprintf("Auxiliary %s%s: request to %s %s (raise auxiliary.%s.%s for slow or reasoning models) on %s, trying fallback",
			auxTaskOrCall(task), tag, auxOrDefault(route.BaseInfo, resolvedProvider), detail, auxTaskOrCall(task), knob, resolvedProvider))
	} else {
		slog.Info(fmt.Sprintf("Auxiliary %s%s: %s on %s (%v), trying fallback", auxTaskOrCall(task), tag, reason, resolvedProvider, firstErr))
	}
	chainFailedModel := route.FinalModel
	if reason == "auth error" || reason == "payment error" {
		chainFailedModel = ""
	}
	var scope FailureScope
	if reason == "payment error" && auxCustomHealthBaseURL(ctx, resolvedProvider, route.BaseInfo) != "" {
		scope = ScopeEndpoint
	}
	failed := auxOrDefault(resolvedProvider, "auto")
	fbClient, fbModel, fbLabel, err := auxTryConfiguredFallbackChainFn(ctx, task, failed, reason, chainFailedModel, route.BaseInfo, scope)
	if err != nil {
		return nil, err
	}
	if fbClient == nil && isAuto {
		if fbClient, fbModel, fbLabel, err = auxTryMainFallbackChainFn(ctx, task, failed, reason, chainFailedModel, route.BaseInfo, scope); err != nil {
			return nil, err
		}
		if fbClient == nil {
			if fbClient, fbModel, fbLabel, err = auxTryPaymentFallbackFn(ctx, resolvedProvider, task, reason, route.BaseInfo, scope, route.MainRuntime); err != nil {
				return nil, err
			}
		}
	} else if fbClient == nil && !explicitAuthWithTaskChain {
		if fbClient, fbModel, fbLabel, err = auxTryMainAgentModelFallbackFn(ctx, resolvedProvider, task, reason, chainFailedModel, route.BaseInfo, scope); err != nil {
			return nil, err
		}
	}
	triedLanes := map[[3]string]bool{}
	var primaryRoute map[string]string
	if route.RouteInfo != nil {
		primaryRoute = make(map[string]string, len(route.RouteInfo))
		for k, v := range route.RouteInfo {
			primaryRoute[k] = v
		}
	}
	for fbClient != nil {
		lane := [3]string{fbLabel, fbModel, fbClient.BaseURL()}
		if triedLanes[lane] {
			break
		}
		triedLanes[lane] = true
		auxRecordRouteInfo(route.RouteInfo, auxFallbackProviderFromLabel(fbLabel), fbModel)
		fbResp, err := yield(auxLadderStep{Kind: "fallback", Args: []any{fbClient, fbModel, fbLabel}})
		if err != nil {
			return nil, err
		}
		if fbResp != nil {
			if route.RouteInfo != nil {
				route.RouteInfo["failed_model"] = route.FinalModel
				route.RouteInfo["failure"] = fmt.Sprintf("%s: %v", reason, firstErr)
				route.RouteInfo["fallback_label"] = fbLabel
			}
			return fbResp, nil
		}
		if fbClient, fbModel, fbLabel, err = auxNextFallbackAfterQuarantine(ctx, task, resolvedProvider, isAuto, route, chainFailedModel, scope, explicitAuthWithTaskChain); err != nil {
			return nil, err
		}
	}
	slog.Warn(fmt.Sprintf("Auxiliary %s%s: %s on %s and all fallbacks exhausted (fallback_chain + main agent model). Raising the primary error.",
		auxTaskOrCall(task), tag, reason, resolvedProvider))
	if primaryRoute != nil && route.RouteInfo != nil {
		for k, v := range primaryRoute {
			route.RouteInfo[k] = v
		}
	}
	return nil, nil
}

// Recovery ladder after the primary request failed, raising the narrowed error when every rung is exhausted
func auxRecoveryLadder(ctx context.Context, firstErr error, req auxPreparedAuxRequest, task string, asyncMode bool, maxTokens any, mainRuntime map[string]any, routeInfo map[string]string) func(yield auxLadderYield) (any, error) {
	return func(yield auxLadderYield) (any, error) {
		tag := ""
		if asyncMode {
			tag = " (async)"
		}
		timeout, _ := auxFloatKwarg("timeout", req.Kwargs["timeout"])
		route := auxLadderRoute{Client: req.Client, Task: task, Tag: tag, AsyncMode: asyncMode, BaseInfo: req.BaseInfo,
			ResolvedProvider: req.ResolvedProvider, ResolvedModel: req.ResolvedModel, ResolvedBaseURL: req.ResolvedBaseURL,
			ResolvedAPIKey: req.ResolvedAPIKey, ResolvedAPIMode: req.ResolvedAPIMode, FinalModel: req.FinalModel,
			MainRuntime: mainRuntime, RouteInfo: routeInfo, TimeoutText: req.EffectiveTimeoutText}
		if timeout != nil {
			route.Timeout = *timeout
		}
		resp, narrowed, kwargs, raised := auxLadderParameterRungs(yield, firstErr, route, req.Kwargs, maxTokens)
		if raised != nil {
			return nil, raised
		}
		if narrowed == nil {
			return resp, nil
		}
		firstErr = narrowed
		clientIsNous := req.ResolvedProvider == "nous" || baseURLHostMatches(req.BaseInfo, "inference-api.nousresearch.com")
		if resp, narrowed, raised = auxLadderNousRungs(ctx, yield, firstErr, route, kwargs, clientIsNous); raised != nil {
			return nil, raised
		} else if narrowed == nil {
			return resp, nil
		}
		firstErr = narrowed
		if resp, narrowed, raised = auxLadderCredentialRungs(ctx, yield, firstErr, route, kwargs, clientIsNous); raised != nil {
			return nil, raised
		} else if narrowed == nil {
			return resp, nil
		}
		firstErr = narrowed
		resp, err := auxLadderProviderFallbackFn(ctx, yield, firstErr, route)
		if err != nil {
			return nil, err
		}
		if resp != nil {
			return resp, nil
		}
		if auxIsConnectionError(firstErr) {
			auxEvictCachedClientInstance(req.Client)
		}
		return nil, firstErr
	}
}

// Sleeps the given seconds unless the context ends first
func auxSleepCtx(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Runs a ladder feeding each performed step's result or error back in, a wait step sleeping
func auxDriveLadder(ctx context.Context, ladder func(yield auxLadderYield) (any, error), perform func(ctx context.Context, step auxLadderStep) (any, error)) (any, error) {
	return ladder(func(step auxLadderStep) (any, error) {
		if step.Kind == "wait" {
			secs, _ := auxFloatKwarg("wait", step.Args[0])
			if secs == nil {
				return nil, auxSleepCtx(ctx, 0)
			}
			return nil, auxSleepCtx(ctx, *secs)
		}
		return perform(ctx, step)
	})
}

// Whole milliseconds between two instants clamped at zero
func auxElapsedMs(startedAt, now time.Time) int {
	return max(0, int(now.Sub(startedAt).Seconds()*1000))
}

// Records the key in latency info the first time it fires
func auxStampLatencyOnce(latency map[string]any, key string, startedAt time.Time) {
	if latency == nil {
		return
	}
	if _, ok := latency[key]; !ok {
		latency[key] = auxElapsedMs(startedAt, time.Now())
	}
}

var auxLatencyMu sync.Mutex

// CallLLM runs an auxiliary LLM request under the configured task limit and relay identity
func CallLLM(ctx context.Context, kwargs map[string]any) (any, error) {
	return auxRelayAuxiliaryCall(ctx, auxRelayCallTask(kwargs), func(ctx context.Context) (any, error) {
		req, err := auxParseCallKwargs("call_llm", kwargs, true)
		if err != nil {
			return nil, err
		}
		return auxCallLLM(ctx, req)
	})
}

// Task label of the relay call record, the text of a truthy task argument
func auxRelayCallTask(kwargs map[string]any) string {
	if task := kwargs["task"]; pyrepr.Truthy(task) {
		return pyrepr.Str(task)
	}
	return ""
}

// Sync call under the task semaphore with latency stamps and the scoped runtime and progress hooks
func auxCallLLM(ctx context.Context, req auxCallRequest) (response any, err error) {
	queueStartedAt := time.Now()
	semaphore, err := auxAcquireSyncAuxSemaphore(ctx, req.Task)
	if err != nil {
		return nil, err
	}
	if semaphore != nil {
		if err := semaphore.Acquire(ctx); err != nil {
			return nil, err
		}
	}
	requestStartedAt := time.Now()
	latency := req.LatencyInfo
	if latency != nil {
		auxLatencyMu.Lock()
		latency["queue_wait_ms"] = auxElapsedMs(queueStartedAt, requestStartedAt)
		auxLatencyMu.Unlock()
	}
	defer func() {
		if latency != nil {
			auxLatencyMu.Lock()
			latency["summary_generation_ms"] = auxElapsedMs(requestStartedAt, time.Now())
			auxLatencyMu.Unlock()
		}
		if semaphore != nil {
			if rerr := semaphore.Release(); rerr != nil && err == nil {
				err = rerr
			}
		}
	}()
	progress := auxHook(ctx, AuxProgressLocal)
	if progress == nil && latency != nil {
		progress = func() error { return nil }
	}
	stamp := func(key string) func() error {
		return func() error {
			auxLatencyMu.Lock()
			defer auxLatencyMu.Unlock()
			auxStampLatencyOnce(latency, key, requestStartedAt)
			return nil
		}
	}
	callCtx, _ := ScopedRuntimeMain(ctx, req.MainRuntime)
	callCtx = AuxProgressHook(callCtx, progress)
	callCtx = AuxThreadLocalHook(callCtx, AuxDispatchLocal, stamp("provider_dispatch_ms"))
	callCtx = AuxThreadLocalHook(callCtx, AuxProviderResponseLocal, stamp("time_to_first_progress_ms"))
	response, err = auxCallImpl(callCtx, req, false)
	if err != nil {
		return nil, err
	}
	if req.Stream && semaphore != nil {
		if stream, ok := response.(AuxChatStream); ok {
			held := semaphore
			semaphore = nil
			return auxReleaseSemaphoreAfterStream(stream, held), nil
		}
	}
	return response, nil
}

type auxSemaphoreStream struct {
	AuxChatStream
	sem  *auxSemaphore
	once sync.Once
}

// Stream that returns its semaphore permit once consumed to the end or closed
func auxReleaseSemaphoreAfterStream(stream AuxChatStream, sem *auxSemaphore) AuxChatStream {
	return &auxSemaphoreStream{AuxChatStream: stream, sem: sem}
}

func (s *auxSemaphoreStream) Next(ctx context.Context) (map[string]any, error) {
	chunk, err := s.AuxChatStream.Next(ctx)
	if err != nil {
		if cerr := s.Close(); cerr != nil {
			return nil, errors.Join(err, cerr)
		}
	}
	return chunk, err
}

func (s *auxSemaphoreStream) Close() error {
	var err error
	s.once.Do(func() {
		cerr := s.AuxChatStream.Close()
		rerr := s.sem.Release()
		err = errors.Join(cerr, rerr)
	})
	return err
}

// AsyncCallLLM runs an auxiliary LLM request on the async client surface under the async task limit
func AsyncCallLLM(ctx context.Context, kwargs map[string]any) (any, error) {
	return auxRelayAuxiliaryCall(ctx, auxRelayCallTask(kwargs), func(ctx context.Context) (any, error) {
		req, err := auxParseCallKwargs("async_call_llm", kwargs, false)
		if err != nil {
			return nil, err
		}
		return auxAsyncCallLLM(ctx, req)
	})
}

// Async call under the async task semaphore with the scoped main runtime bound
func auxAsyncCallLLM(ctx context.Context, req auxCallRequest) (response any, err error) {
	semaphore, err := auxAcquireAsyncAuxSemaphore(ctx, req.Task)
	if err != nil {
		return nil, err
	}
	if semaphore != nil {
		if err := semaphore.Acquire(ctx); err != nil {
			return nil, err
		}
		defer func() {
			if rerr := semaphore.Release(); rerr != nil && err == nil {
				err = rerr
			}
		}()
	}
	callCtx, _ := ScopedRuntimeMain(ctx, req.MainRuntime)
	return auxCallImpl(callCtx, req, true)
}

// Prepared request with the same provider retry and fallback candidate fields bundled
func auxPlanAuxCall(ctx context.Context, req auxCallRequest, asyncMode bool) (auxPreparedAuxRequest, auxSameProviderRetry, auxFallbackRequest, error) {
	req.MainRuntime = auxNormalizeMainRuntime(ctx, req.MainRuntime)
	prep, err := auxPrepareAuxRequest(ctx, req, asyncMode)
	if err != nil {
		return prep, auxSameProviderRetry{}, auxFallbackRequest{}, err
	}
	candidate := auxFallbackRequest{Task: req.Task, Messages: auxDictList(req.Messages), Tools: auxDictList(req.Tools), Temperature: req.Temperature,
		MaxTokens: req.MaxTokens, EffectiveTimeout: prep.EffectiveTimeout, EffectiveExtraBody: prep.EffectiveExtraBody, ReasoningConfig: req.ReasoningConfig}
	retry := auxSameProviderRetry{Task: req.Task, ResolvedBaseURL: prep.ResolvedBaseURL, ResolvedAPIKey: prep.ResolvedAPIKey, ResolvedAPIMode: prep.ResolvedAPIMode,
		MainRuntime: req.MainRuntime, FinalModel: prep.FinalModel, Messages: req.Messages, Temperature: req.Temperature, MaxTokens: req.MaxTokens,
		Tools: req.Tools, EffectiveTimeout: prep.EffectiveTimeout, EffectiveExtraBody: prep.EffectiveExtraBody, ReasoningConfig: req.ReasoningConfig,
		ExtraHeaders: req.ExtraHeaders}
	return prep, retry, candidate, nil
}

// Transient transport blip worth a same provider retry, critical path timeouts going straight to fallback
func auxShouldRetrySameProvider(task string, err error, tag string) bool {
	if !auxIsTransientTransportError(err) {
		return false
	}
	if auxShouldSkipSameProviderRetry(task, err) {
		slog.Info(fmt.Sprintf("Auxiliary %s%s: timeout on the critical path; skipping same-provider retry and falling back: %v", task, tag, err))
		return false
	}
	return true
}

// Sync or async call with the transient retry and the recovery ladder over the prepared request
func auxCallImpl(ctx context.Context, req auxCallRequest, asyncMode bool) (any, error) {
	if asyncMode {
		req.ExtraHeaders, req.APIMode = nil, ""
	}
	prep, retry, candidate, err := auxPlanAuxCall(ctx, req, asyncMode)
	if err != nil {
		return nil, err
	}
	task, client, kwargs, requestProvider := req.Task, prep.Client, prep.Kwargs, prep.RequestProvider
	if req.Stream && !asyncMode {
		kwargs["stream"] = true
		if len(req.StreamOptions) > 0 {
			kwargs["stream_options"] = req.StreamOptions
		}
		if _, codex := client.(*CodexAuxiliaryClient); codex && task == "moa_aggregator" {
			return client.CreateChatCompletion(ctx, kwargs)
		}
		return auxRelaySyncStreamFn(ctx, client, kwargs, requestProvider, prep.ResolvedAPIMode)
	}
	forceStream := auxProviderRequiresStream(ctx, auxOrDefault(prep.BaseInfo, prep.ResolvedBaseURL))
	primary := func(validateProvider, validateBaseURL string) (any, error) {
		var resp any
		var err error
		if asyncMode {
			resp, err = auxRelayAsyncCompletion(ctx, client, kwargs, requestProvider, prep.ResolvedAPIMode, func(ctx context.Context, request map[string]any) (any, error) {
				return auxAcreateWithProgress(ctx, client, request, task, forceStream)
			})
		} else {
			resp, err = auxRelaySyncCompletionFn(ctx, client, kwargs, requestProvider, prep.ResolvedAPIMode, func(ctx context.Context, request map[string]any) (any, error) {
				return auxCreateWithProgress(ctx, client, request, task, forceStream)
			})
		}
		if err != nil {
			return nil, err
		}
		return auxValidateLLMResponseFn(ctx, resp, task, validateProvider, validateBaseURL)
	}
	resp, firstErr := primary(requestProvider, prep.BaseInfo)
	if firstErr == nil {
		return resp, nil
	}
	tag := ""
	if asyncMode {
		tag = " (async)"
	}
	if auxShouldRetrySameProvider(task, firstErr, tag) {
		if asyncMode {
			slog.Info(fmt.Sprintf("Auxiliary %s (async): transient transport error; retrying once on the same provider before fallback: %v", auxTaskOrCall(task), firstErr))
			resp, firstErr = primary("", "")
		} else {
			resp, firstErr = auxTransientRetries(ctx, task, firstErr, func() (any, error) { return primary("", "") })
		}
		if firstErr == nil {
			return resp, nil
		}
	}
	perform := func(ctx context.Context, step auxLadderStep) (any, error) {
		switch step.Kind {
		case "call":
			stepClient, _ := step.Args[0].(AuxClient)
			stepKwargs, _ := step.Args[1].(map[string]any)
			var resp any
			var err error
			if asyncMode {
				resp, err = auxRelayAsyncCompletion(ctx, stepClient, stepKwargs, prep.ResolvedProvider, prep.ResolvedAPIMode, nil)
			} else {
				resp, err = auxRelaySyncCompletionFn(ctx, stepClient, stepKwargs, prep.ResolvedProvider, prep.ResolvedAPIMode, nil)
			}
			if err != nil {
				return nil, err
			}
			return auxValidateLLMResponseFn(ctx, resp, task, "", "")
		case "retry_same_provider":
			r := retry
			r.ResolvedProvider, _ = step.Args[0].(string)
			r.ResolvedModel, _ = step.Args[1].(string)
			if asyncMode {
				return auxRetrySameProviderAsync(ctx, r)
			}
			return auxRetrySameProviderSync(ctx, r)
		}
		fbClient, _ := step.Args[0].(AuxClient)
		fbModel, _ := step.Args[1].(string)
		fbLabel, _ := step.Args[2].(string)
		if asyncMode {
			fbClient, _ = auxToAsyncClientFn(ctx, fbClient, fbModel, task == "vision")
		}
		return auxCallFallbackCandidate(ctx, fbClient, fbModel, fbLabel, candidate, asyncMode)
	}
	return auxDriveLadder(ctx, auxRecoveryLadder(ctx, firstErr, prep, task, asyncMode, retry.MaxTokens, retry.MainRuntime, req.RouteInfo), perform)
}

// Bounded same provider retries with exponential backoff, the last transient error raised when all fail
func auxTransientRetries(ctx context.Context, task string, transientErr error, primary func() (any, error)) (any, error) {
	maxRetries := auxTransientRetryCount(ctx)
	last := transientErr
	for attempt := 1; attempt <= maxRetries; attempt++ {
		backoff := math.Min(auxTransientRetryBackoffBase*math.Pow(2.0, float64(attempt-1)), 8.0)
		slog.Info(fmt.Sprintf("Auxiliary %s: transient transport error (attempt %d/%d); retrying same provider after %.1fs before fallback: %v",
			auxTaskOrCall(task), attempt, maxRetries, backoff, last))
		if err := auxSleepCtx(ctx, backoff); err != nil {
			return nil, err
		}
		resp, err := primary()
		if err == nil {
			return resp, nil
		}
		if !auxIsTransientTransportError(err) {
			return nil, err
		}
		last = err
	}
	return nil, last
}
