package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Runtime provider resolution seam for an explicitly requested provider
var auxResolveRuntimeProviderFn = func(ctx context.Context, requested string) (*Runtime, error) {
	return modelsResolver(ctx).ResolveRuntime(requested, "", "", nil, "")
}

// Azure Foundry runtime resolution seam over the main resolver
var auxResolveAzureFoundryRuntimeFn = func(ctx context.Context, modelCfg map[string]any, explicitAPIKey, explicitBaseURL, targetModel string) (*Runtime, error) {
	return modelsResolver(ctx).resolveAzureFoundryRuntime("azure-foundry", modelCfg, explicitAPIKey, nil, explicitBaseURL, targetModel)
}

// Seams the hermes tests patch as module attributes
var (
	auxResolveCustomRuntimeFn  = auxResolveCustomRuntime
	auxBuildAnthropicClientFn  = buildAnthropicClient
	auxSelectAnthropicEntryFn  = auxSelectPoolEntry
	auxResolveAnthropicTokenFn = func(ctx context.Context) string { return ResolveAnthropicToken(modelsPoolEnv(ctx), "") }
)

// Active custom endpoint base URL, key and api mode like the main CLI, empty when none is configured
func auxResolveCustomRuntime(ctx context.Context) (string, string, string, error) {
	var rt *Runtime
	var rerr error
	if perr := auxRecoverRaise(func() { rt, rerr = auxResolveRuntimeProviderFn(ctx, "custom") }); perr != nil {
		rerr = perr
	}
	var authErr *AuthError
	if rerr != nil && errors.As(rerr, &authErr) {
		slog.Debug(fmt.Sprintf("Auxiliary client: no custom endpoint configured: %v", rerr))
		return "", "", "", nil
	}
	if rerr != nil {
		slog.Debug(fmt.Sprintf("Auxiliary client: custom runtime resolution failed: %v", rerr))
		rt = nil
	}
	var base, key, mode string
	if rt == nil {
		openaiBase, err := auxScopedKeyEnv(ctx, "OPENAI_BASE_URL")
		if err != nil {
			return "", "", "", err
		}
		if openaiBase = strings.TrimRight(openaiBase, "/"); openaiBase == "" {
			return "", "", "", nil
		}
		if key, err = auxScopedKeyEnv(ctx, "OPENAI_API_KEY"); err != nil {
			return "", "", "", err
		}
		base = openaiBase
	} else {
		base, key, mode = rt.BaseURL, rt.APIKey, string(rt.APIMode)
	}
	if pystr.Strip(base) == "" {
		return "", "", "", nil
	}
	base = strings.TrimRight(pystr.Strip(base), "/")
	if baseURLHostMatches(base, "openrouter.ai") {
		return "", "", "", nil
	}
	if pystr.Strip(key) == "" {
		key = "no-key-required"
	}
	if pystr.Strip(mode) == "" {
		mode = ""
	}
	return base, pystr.Strip(key), mode, nil
}

// Base URL of the active custom endpoint, empty when none, a scope failure raising as GetSecret does
func auxCurrentCustomBaseURL(ctx context.Context) string {
	base, _, _, err := auxResolveCustomRuntimeFn(ctx)
	if err != nil {
		panic(err)
	}
	return base
}

var auxProxyEnvKeys = []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "https_proxy", "http_proxy", "all_proxy"}

// Fails fast on a malformed proxy env URL after normalizing the proxy variables in place
func auxValidateProxyEnvURLs() error {
	oauthNormalizeProxyEnvVars()
	for _, key := range auxProxyEnvKeys {
		value := pystr.Strip(os.Getenv(key))
		if value == "" {
			continue
		}
		parsed, err := pyurl.Parse(value)
		if err == nil && parsed.Scheme != "" {
			_, _, err = parsed.Port()
		}
		if err != nil {
			return &AuxRuntimeError{Message: fmt.Sprintf("Malformed proxy environment variable %s=%s. "+
				"Fix or unset your proxy settings and try again.", key, pyrepr.Repr(value))}
		}
	}
	return nil
}

// Rejects an http or https custom endpoint URL whose port cannot be parsed
func auxValidateBaseURL(baseURL string) error {
	candidate := pystr.Strip(baseURL)
	if candidate == "" || strings.HasPrefix(candidate, "acp://") {
		return nil
	}
	parsed, err := pyurl.Parse(candidate)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		_, _, err = parsed.Port()
	}
	if err != nil {
		return &AuxRuntimeError{Message: fmt.Sprintf("Malformed custom endpoint URL: %s. "+
			"Run `icebath setup` or `icebath model` and enter a valid http(s) base URL.", pyrepr.Repr(candidate))}
	}
	return nil
}

// Codex adapter over an SDK client or the availability probe client
func auxWrapCodex(client AuxClient, model string) *CodexAuxiliaryClient {
	return NewCodexAuxiliaryClient(client.(auxResponsesClient), model)
}

// Client for the active custom endpoint on its declared wire, or the Messages wrap a URL implies
func auxTryCustomEndpoint(ctx context.Context) (AuxClient, string, error) {
	base, key, mode, err := auxResolveCustomRuntimeFn(ctx)
	if err != nil || base == "" || key == "" {
		return nil, "", err
	}
	if strings.HasPrefix(strings.ToLower(base), strings.ToLower(DefaultCodexBaseURL)) {
		return nil, "", nil
	}
	model := auxReadMainModelForAux(ctx)
	if model == "" {
		model = "gpt-4o-mini"
	}
	shownMode := mode
	if shownMode == "" {
		shownMode = "chat_completions"
	}
	slog.Debug(fmt.Sprintf("Auxiliary client: custom endpoint (%s, api_mode=%s)", model, shownMode))
	cleanBase, query, err := auxExtractURLQueryParams(base)
	if err != nil {
		return nil, "", err
	}
	opts := auxOpenAIOptions{}
	if len(query) > 0 {
		opts.DefaultQuery = query
	}
	if headers := auxApplyUserDefaultHeaders(ctx, nil); len(headers) > 0 {
		opts.DefaultHeaders = headers
	}
	switch mode {
	case "codex_responses":
		return auxWrapCodex(auxCreateOpenAIClient(ctx, key, cleanBase, opts), model), model, nil
	case "anthropic_messages":
		real, err := auxBuildAnthropicClientFn(ctx, key, base, 0, false)
		if err != nil {
			return nil, "", err
		}
		isOAuth := AnthropicRouteIsOAuth(ctx, base, key, nil, "")
		return newAnthropicAuxiliaryClientFrom(real, model, key, base, isOAuth), model, nil
	}
	wrapped, err := auxMaybeWrapAnthropic(ctx, auxCreateOpenAIClient(ctx, key, cleanBase, opts), model, key, base, mode)
	if err != nil {
		return nil, "", err
	}
	return wrapped, model, nil
}

// Codex adapter for xAI Grok OAuth on the Responses API, nil without a model or an xAI login
func auxBuildXaiOAuthAuxClient(ctx context.Context, model string) (AuxClient, string, error) {
	if model == "" {
		slog.Warn("Auxiliary client: xai-oauth requested without a model; " +
			"pass model explicitly (auxiliary.<task>.model in config.yaml).")
		return nil, "", nil
	}
	apiKey, baseURL, ok, err := auxResolveXaiOAuthForAux(ctx)
	if err != nil || !ok {
		return nil, "", err
	}
	slog.Debug(fmt.Sprintf("Auxiliary client: xAI OAuth (%s via Responses API)", model))
	real := auxCreateOpenAIClient(ctx, apiKey, baseURL, auxOpenAIOptions{DefaultHeaders: tools.XAIDefaultHeaders()})
	return auxWrapCodex(real, model), model, nil
}

// Profile scoped Codex base URL override without a trailing slash
func auxCodexBaseURLOverride(ctx context.Context) (string, error) {
	value, err := auxScopedKeyEnv(ctx, "ICEBATH_CODEX_BASE_URL")
	return strings.TrimRight(value, "/"), err
}

// Codex adapter for an explicit model, nil without a model or a Codex OAuth token
func auxBuildCodexClient(ctx context.Context, model string) (AuxClient, string, error) {
	if model == "" {
		slog.Warn("Auxiliary client: openai-codex requested without a model; " +
			"pass model explicitly (auxiliary.<task>.model in config.yaml).")
		return nil, "", nil
	}
	token, baseURL, err := auxResolveCodexCredentialAndBase(ctx)
	if err != nil || token == "" {
		return nil, "", err
	}
	slog.Debug(fmt.Sprintf("Auxiliary client: Codex OAuth (%s via Responses API)", model))
	real := auxCreateOpenAIClient(ctx, token, baseURL, auxOpenAIOptions{DefaultHeaders: codexCloudflareHeaders(token, baseURL)})
	return auxWrapCodex(real, model), model, nil
}

// Azure Foundry client from the main runtime resolver on its per model wire, nil when unresolvable
func auxTryAzureFoundry(ctx context.Context, model, explicitAPIKey, explicitBaseURL, apiMode string) (AuxClient, string, error) {
	modelCfg := map[string]any{}
	if cfg, err := auxLoadConfigReadonlyFn(ctx); err == nil {
		if m, ok := cfg["model"].(map[string]any); ok {
			modelCfg = m
		}
	}
	var rt *Runtime
	var rerr error
	if perr := auxRecoverRaise(func() {
		rt, rerr = auxResolveAzureFoundryRuntimeFn(ctx, modelCfg, explicitAPIKey, explicitBaseURL, model)
	}); perr != nil {
		rerr = perr
	}
	var authErr *AuthError
	if rerr != nil {
		if errors.As(rerr, &authErr) {
			slog.Debug(fmt.Sprintf("Auxiliary azure-foundry: %v", rerr))
		} else {
			slog.Debug(fmt.Sprintf("Auxiliary azure-foundry runtime error: %v", rerr))
		}
		return nil, "", nil
	}
	var apiKey any = rt.APIKey
	if rt.APIKey == "" && rt.TokenSource != nil {
		src := rt.TokenSource
		apiKey = func() (string, error) { return src(ctx) }
	}
	baseURL := rt.BaseURL
	runtimeAPIMode := apiMode
	if runtimeAPIMode == "" {
		runtimeAPIMode = string(rt.APIMode)
	}
	if runtimeAPIMode == "" {
		runtimeAPIMode = "chat_completions"
	}
	if apiKey == "" || baseURL == "" {
		return nil, "", nil
	}
	requested := model
	if requested == "" {
		if v := modelCfg["default"]; pyrepr.Truthy(v) {
			requested = pyrepr.Str(v)
		}
	}
	finalModel := auxNormalizeResolvedModel(requested, "azure-foundry")
	if finalModel == "" {
		slog.Debug(fmt.Sprintf("Auxiliary azure-foundry: no model resolved (model=%s, default=%s)",
			pyrepr.Repr(auxNoneIfEmpty(model)), pyrepr.Repr(modelCfg["default"])))
		return nil, "", nil
	}
	cleanBase, query, err := auxExtractURLQueryParams(baseURL)
	if err != nil {
		return nil, "", err
	}
	opts := auxOpenAIOptions{}
	if len(query) > 0 {
		opts.DefaultQuery = query
	}
	client := auxCreateOpenAIClient(ctx, apiKey, cleanBase, opts)
	switch runtimeAPIMode {
	case "codex_responses":
		return auxWrapCodex(client, finalModel), finalModel, nil
	case "anthropic_messages":
		wrapped, err := auxMaybeWrapAnthropic(ctx, client, finalModel, apiKey, baseURL, runtimeAPIMode)
		if err != nil {
			return nil, "", err
		}
		return wrapped, finalModel, nil
	}
	return client, finalModel, nil
}

// Nil for an empty string so repr prints None as hermes does for a missing value
func auxNoneIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Python str of a truthy value, empty for a falsy one
func auxOrEmptyStr(v any) string {
	if pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Truthiness of a string or callable api key
func auxKeyTruthy(key any) bool {
	switch k := key.(type) {
	case nil:
		return false
	case string:
		return k != ""
	}
	return true
}

// Native Anthropic client from the pool, explicit or standalone token at a compatible host, nil when unavailable
func auxTryAnthropic(ctx context.Context, explicitAPIKey any, explicitBaseURL string) (AuxClient, string, error) {
	poolPresent, entry := auxSelectAnthropicEntryFn(ctx, "anthropic")
	var token any
	switch {
	case auxKeyTruthy(explicitAPIKey):
		token = explicitAPIKey
	case poolPresent && entry != nil:
		token = auxPoolRuntimeAPIKey(entry)
	default:
		entry = nil
		token = auxResolveAnthropicTokenFn(ctx)
	}
	if !auxKeyTruthy(token) {
		return nil, "", nil
	}
	baseURL := auxAnthropicDefaultBaseURL
	if poolPresent {
		baseURL = auxPoolRuntimeBaseURL(ctx, entry, auxAnthropicDefaultBaseURL)
	}
	if cfg, err := auxLoadConfigReadonlyFn(ctx); err == nil {
		if modelCfg, ok := cfg["model"].(map[string]any); ok && pystr.Lower(pystr.Strip(auxOrEmptyStr(modelCfg["provider"]))) == "anthropic" {
			if raw, ok := modelCfg["base_url"].(string); ok || !pyrepr.Truthy(modelCfg["base_url"]) {
				cfgBaseURL := strings.TrimRight(pystr.Strip(raw), "/")
				if cfgBaseURL != "" && auxIsAnthropicCompatibleHost(cfgBaseURL) {
					baseURL = cfgBaseURL
				}
			}
		}
	}
	if overrideURL := strings.TrimRight(pystr.Strip(explicitBaseURL), "/"); overrideURL != "" {
		if !auxIsAnthropicCompatibleHost(overrideURL) {
			slog.Warn(fmt.Sprintf("Auxiliary client: refusing anthropic explicit base_url %s — not an "+
				"Anthropic-compatible host; no client built and no request sent.", pyrepr.Repr(overrideURL)))
			return nil, "", nil
		}
		baseURL = overrideURL
	}
	tokenStr, isStr := token.(string)
	if !isStr {
		return nil, "", &auxAttributeError{typeName: pyrepr.TypeName(token), attr: "startswith"}
	}
	isOAuth := isOAuthToken(tokenStr)
	model := auxGetAuxModelForProvider(ctx, "anthropic")
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	if auxProbeActive(ctx) {
		return &auxProbeClient{apiKey: "", baseURL: baseURL}, model, nil
	}
	slog.Debug(fmt.Sprintf("Auxiliary client: Anthropic native (%s) at %s (oauth=%s)", model, baseURL, pyrepr.Str(isOAuth)))
	real, err := auxBuildAnthropicClientFn(ctx, token, baseURL, 0, false)
	if err != nil {
		return nil, "", err
	}
	return newAnthropicAuxiliaryClientFrom(real, model, token, baseURL, isOAuth), model, nil
}

// One labelled rung of the auto detection chain
type auxProviderChainEntry struct {
	label string
	try   func(ctx context.Context) (AuxClient, string, error)
}

// Ordered auto detection chain resolved at call time, openai-codex deliberately absent
func auxGetProviderChain() []auxProviderChainEntry {
	return []auxProviderChainEntry{
		{"openrouter", func(ctx context.Context) (AuxClient, string, error) { return auxTryOpenrouter(ctx, nil, "", "") }},
		{"nous", func(ctx context.Context) (AuxClient, string, error) { return auxTryNous(ctx, false) }},
		{"local/custom", auxTryCustomEndpoint},
		{"api-key", auxResolveAPIKeyProvider},
	}
}
