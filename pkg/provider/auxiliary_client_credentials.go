package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Pool entry selector seam the tests swap for a present but exhausted pool
var auxSelectPoolEntryFn = auxSelectPoolEntry

// Trimmed slashless form of a URL candidate
func auxTrimURL(v string) string {
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// Fresh xAI OAuth key and base URL of the selected xAI login, false when there is none to use
func auxResolveXaiOAuthForAux(ctx context.Context) (string, string, bool) {
	var apiKey, baseURL string
	err := auxRecoverRaise(func() {
		var poolErr error
		apiKey, baseURL, poolErr = auxXaiOAuthPoolPair(ctx)
		if poolErr != nil {
			panic(poolErr)
		}
	})
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Auxiliary xAI OAuth credential resolution failed: %v", err))
		return "", "", false
	}
	return apiKey, baseURL, apiKey != "" && baseURL != ""
}

// Key and validated base URL of the selected xAI OAuth pool row, scoped base URL env first
func auxXaiOAuthPoolPair(ctx context.Context) (string, string, error) {
	env := modelsPoolEnv(ctx)
	pool := LoadSelectedPool(env, "xai-oauth")
	if pool == nil || !pool.HasCredentials() {
		return "", "", nil
	}
	entry, found := pool.Select("")
	if !found {
		return "", "", nil
	}
	apiKey := strings.TrimSpace(entry.RuntimeAPIKey())
	if apiKey == "" {
		apiKey = strings.TrimSpace(entry.AccessToken)
	}
	candidate := ""
	for _, name := range []string{"ICEBATH_XAI_BASE_URL", "XAI_BASE_URL"} {
		value, err := auxScopedKeyEnv(ctx, name)
		if err != nil {
			return "", "", err
		}
		if candidate = auxTrimURL(value); candidate != "" {
			break
		}
	}
	if candidate == "" {
		candidate = auxTrimURL(entry.RuntimeBaseURL())
	}
	if candidate == "" {
		candidate = auxTrimURL(entry.BaseURL)
	}
	return apiKey, xaiValidateInferenceBaseURL(env, candidate, DefaultXAIOAuthBaseURL), nil
}

// Codex token of the selected login and the base URL of the same authority, an override first, else the route of the login
func auxResolveCodexCredentialAndBase(ctx context.Context) (string, string, error) {
	override, err := auxCodexBaseURLOverride(ctx)
	if err != nil {
		return "", "", err
	}
	present, entry := auxSelectPoolEntryFn(ctx, "openai-codex")
	if present {
		if token := auxPoolRuntimeAPIKey(entry); token != "" {
			if override != "" {
				return token, override, nil
			}
			return token, codexPoolRouteBaseURL(modelsPoolEnv(ctx), auxPoolRuntimeBaseURL(ctx, entry, "")), nil
		}
	}
	if override == "" {
		override = DefaultCodexBaseURL
	}
	return "", override, nil
}

// ResolveCodexCredentialAndBase is the Codex token and the base url of the same authority
func ResolveCodexCredentialAndBase(ctx context.Context) (string, string, error) {
	return auxResolveCodexCredentialAndBase(ctx)
}

// Registry rows the API key chain walks, swapped by tests
var auxRegistryRowsFn = canonicalRegistryRows

// Wire client of the first healthy API key provider with credentials and a known auxiliary model
func auxResolveAPIKeyProvider(ctx context.Context) (AuxClient, string, error) {
	env := modelsPoolEnv(ctx)
	for _, pconfig := range auxRegistryRowsFn(ctx) {
		providerID := pconfig.Name
		if pconfig.AuthType != AuthTypeAPIKey {
			continue
		}
		if auxIsProviderUnhealthy(ctx, providerID, "") {
			pkgLog().Debug(fmt.Sprintf("Auxiliary api-key chain: %s is unhealthy, skipping", providerID))
			continue
		}
		if providerID == "anthropic" {
			if !IsProviderExplicitlyConfigured(env, "anthropic") {
				continue
			}
			return auxTryAnthropic(ctx, nil, "")
		}
		if profile := getProviderProfile(ctx, providerID); profile != nil && profile.AmbientCredentials {
			if !IsProviderExplicitlyConfigured(env, providerID) {
				continue
			}
		}
		model := auxGetAuxModelForProvider(ctx, providerID)
		if model == "" {
			continue
		}
		var apiKey any
		var rawBaseURL, via string
		present, entry := auxSelectPoolEntryFn(ctx, providerID)
		if present {
			key := auxPoolRuntimeAPIKey(entry)
			if key == "" {
				continue
			}
			apiKey = key
			rawBaseURL = auxPoolRuntimeBaseURL(ctx, entry, pconfig.BaseURL)
			if rawBaseURL == "" {
				rawBaseURL = pconfig.BaseURL
			}
			via = " via pool"
		} else {
			creds, err := auxResolveAPIKeyProviderCredentialsFn(ctx, providerID)
			if err != nil {
				return nil, "", err
			}
			key := strings.TrimSpace(creds.APIKey)
			if key == "" {
				continue
			}
			apiKey = key
			rawBaseURL = strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
			if rawBaseURL == "" {
				rawBaseURL = pconfig.BaseURL
			}
		}
		runtime := auxNormalizeMainRuntime(ctx, nil)
		if rtBase, _ := runtime["base_url"].(string); runtime["provider"] == providerID && rtBase != "" {
			rawBaseURL = strings.TrimRight(rtBase, "/")
			if rtKey, ok := runtime["api_key"].(string); ok && rtKey != "" {
				apiKey = rtKey
			}
			via = " (session endpoint)"
		}
		name := pconfig.DisplayName
		if name == "" {
			name = providerID
		}
		pkgLog().Debug(fmt.Sprintf("Auxiliary text client: %s (%s)%s", name, model, via))
		baseURL := auxToOpenAIBaseURL(rawBaseURL)
		keyStr, _ := apiKey.(string)
		if providerID == "gemini" && isNativeGeminiBaseURL(baseURL) {
			client, err := newAuxGeminiNativeClient(keyStr, baseURL, nil)
			if err != nil {
				return nil, "", err
			}
			return client, model, nil
		}
		var headers map[string]string
		switch {
		case baseURLHostMatches(baseURL, "api.kimi.com"):
			headers = map[string]string{"User-Agent": "claude-code/0.1.0"}
		case baseURLHostMatches(baseURL, "integrate.api.nvidia.com"):
			headers = BuildNvidiaNIMHeaders(baseURL)
		default:
			if headers = auxRouteClientHeaders(ctx, baseURL, providerID, false); len(headers) == 0 {
				headers = auxProfileDefaultHeaders(ctx, providerID)
			}
		}
		opts := auxOpenAIOptions{}
		if len(headers) > 0 {
			opts.DefaultHeaders = headers
		}
		if merged := auxApplyUserDefaultHeaders(ctx, opts.DefaultHeaders); len(merged) > 0 {
			opts.DefaultHeaders = merged
		}
		client := auxCreateOpenAIClient(ctx, apiKey, baseURL, opts)
		wrapped, err := auxMaybeWrapAnthropic(ctx, client, model, apiKey, rawBaseURL, "")
		if err != nil {
			return nil, "", err
		}
		return wrapped, model, nil
	}
	return nil, "", nil
}

// Client headers a route profile answers for the endpoint, nil when no profile claims it
func auxRouteClientHeaders(ctx context.Context, baseURL, provider string, isVision bool) map[string]string {
	answer := routeAnswer(ctx, "client_headers", provider, nil, map[string]any{"base_url": baseURL, "has_images": isVision})
	return auxHeaderMap(answer)
}

// String header map of a hook answer, nil for a falsy answer
func auxHeaderMap(answer any) map[string]string {
	switch h := answer.(type) {
	case map[string]string:
		if len(h) == 0 {
			return nil
		}
		return copyStringMap(h)
	case map[string]any:
		if len(h) == 0 {
			return nil
		}
		out := make(map[string]string, len(h))
		for k, v := range h {
			out[k] = anyutil.String(v)
		}
		return out
	}
	return nil
}

// Endpoint specific client headers by host merged with the user default headers, nil when none apply
func auxEndpointDefaultHeaders(ctx context.Context, baseURL, provider string, isVision, xai bool) map[string]string {
	var headers map[string]string
	if baseURLHostMatches(baseURL, "api.kimi.com") {
		headers = map[string]string{"User-Agent": "claude-code/0.1.0"}
	} else if route := auxRouteClientHeaders(ctx, baseURL, provider, isVision); len(route) > 0 {
		headers = route
	} else if baseURLHostMatches(baseURL, "integrate.api.nvidia.com") {
		headers = BuildNvidiaNIMHeaders(baseURL)
	} else if xai && baseURLHostMatches(baseURL, "x.ai") {
		headers = tools.XAIDefaultHeaders()
	} else {
		headers = auxProfileDefaultHeaders(ctx, provider)
	}
	if len(headers) == 0 {
		headers = nil
	}
	merged := auxApplyUserDefaultHeaders(ctx, headers)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// Attribution headers of the provider profile, nil without a provider or profile headers
func auxProfileDefaultHeaders(ctx context.Context, provider string) map[string]string {
	if provider == "" {
		return nil
	}
	var headers map[string]string
	_ = auxRecoverRaise(func() {
		if profile := getProviderProfile(ctx, provider); profile != nil {
			if resolved := profile.DefaultHeaders(); len(resolved) > 0 {
				headers = resolved
			}
		}
	})
	return headers
}

// OpenRouter models whose paid lane warning was already logged
var (
	auxPaidLaneWarnedMu sync.Mutex
	auxPaidLaneWarned   = map[string]bool{}
)

// True for a free SKU by its free suffix or stealth prefix
func auxIsFreeModel(model string) bool {
	if model == "" {
		return false
	}
	normalized := strings.TrimSpace(model)
	return strings.HasSuffix(normalized, ":free") || strings.HasPrefix(normalized, "stealth/")
}

// Free only flag and OpenRouter model of the auxiliary config, the built-in free model when none is set
func auxOpenrouterSettings(ctx context.Context) (bool, string) {
	model := strings.TrimSpace(settings.AuxiliaryOpenrouterModel.Get(ctx))
	if model == "" {
		model = auxOpenRouterModel
	}
	return settings.AuxiliaryFreeOnly.Get(ctx), model
}

// Warns once per model that a non free OpenRouter model may incur spend
func auxWarnPaidLaneOnce(model string) {
	auxPaidLaneWarnedMu.Lock()
	seen := auxPaidLaneWarned[model]
	auxPaidLaneWarned[model] = true
	auxPaidLaneWarnedMu.Unlock()
	if seen {
		return
	}
	pkgLog().Warn(fmt.Sprintf("Auxiliary client: PAID lane engaged for auxiliary task — OpenRouter fallback model %s is not "+
		"a :free SKU and may incur real spend. Set auxiliary.free_only: true to restrict auxiliary "+
		"fallbacks to free models, or auxiliary.openrouter_model to a :free model.", anyutil.Quote(model)))
}

// OpenRouter client from an explicit key, the pool or OPENROUTER_API_KEY, honouring the free only policy
func auxTryOpenrouter(ctx context.Context, explicitAPIKey any, model, explicitBaseURL string) (AuxClient, string, error) {
	freeOnly, cfgModel := auxOpenrouterSettings(ctx)
	orModel := model
	if orModel == "" {
		orModel = cfgModel
	}
	if freeOnly && !auxIsFreeModel(orModel) {
		pkgLog().Warn(fmt.Sprintf("Auxiliary client: auxiliary.free_only is enabled but the OpenRouter fallback model %s is "+
			"not a :free SKU — skipping the OpenRouter fallback. Set auxiliary.openrouter_model to a "+
			":free model (e.g. nvidia/nemotron-3-ultra-550b-a55b:free) or disable auxiliary.free_only.", anyutil.Quote(orModel)))
		return nil, "", nil
	}
	if !auxIsFreeModel(orModel) {
		auxWarnPaidLaneOnce(orModel)
	}
	overrideURL := strings.TrimRight(strings.TrimSpace(explicitBaseURL), "/")
	present, entry := auxSelectPoolEntryFn(ctx, "openrouter")
	if present {
		orKey := explicitAPIKey
		if !auxKeyTruthy(orKey) {
			orKey = auxPoolRuntimeAPIKey(entry)
		}
		if auxKeyTruthy(orKey) {
			baseURL := overrideURL
			if baseURL == "" {
				baseURL = auxPoolRuntimeBaseURL(ctx, entry, OpenRouterBaseURL)
			}
			if baseURL == "" {
				baseURL = OpenRouterBaseURL
			}
			pkgLog().Debug("Auxiliary client: OpenRouter via pool")
			return auxCreateOpenAIClient(ctx, orKey, baseURL, auxOpenAIOptions{DefaultHeaders: BuildORHeaders(ctx)}), orModel, nil
		}
		pkgLog().Debug("Auxiliary client: OpenRouter pool exhausted, trying OPENROUTER_API_KEY")
	}
	orKey := explicitAPIKey
	if !auxKeyTruthy(orKey) {
		envKey, err := auxScopedKeyEnv(ctx, "OPENROUTER_API_KEY")
		if err != nil {
			return nil, "", err
		}
		orKey = envKey
	}
	if !auxKeyTruthy(orKey) {
		reason, err := auxDescribeOpenrouterUnavailable(ctx, orModel)
		if err != nil {
			return nil, "", err
		}
		auxMarkProviderUnhealthy(ctx, "openrouter", 60, "", reason, slog.LevelDebug)
		return nil, "", nil
	}
	pkgLog().Debug("Auxiliary client: OpenRouter")
	headers := BuildORHeaders(ctx)
	baseURL := overrideURL
	if baseURL == "" {
		baseURL = OpenRouterBaseURL
	}
	return auxCreateOpenAIClient(ctx, orKey, baseURL, auxOpenAIOptions{DefaultHeaders: headers}), orModel, nil
}

// Policy or credential reason the OpenRouter route was unavailable
func auxDescribeOpenrouterUnavailable(ctx context.Context, model string) (string, error) {
	freeOnly, cfgModel := auxOpenrouterSettings(ctx)
	orModel := model
	if orModel == "" {
		orModel = cfgModel
	}
	if freeOnly && !auxIsFreeModel(orModel) {
		return fmt.Sprintf("auxiliary.free_only rejected non-free model %s; "+
			"the request was skipped before provider availability checks", anyutil.Quote(orModel)), nil
	}
	present, entry := auxSelectPoolEntryFn(ctx, "openrouter")
	if present {
		if entry == nil {
			return "OpenRouter credential pool has no usable entries (credentials may be exhausted)", nil
		}
		if auxPoolRuntimeAPIKey(entry) == "" {
			return "OpenRouter credential pool entry is missing a runtime API key", nil
		}
	}
	envKey, err := auxScopedKeyEnv(ctx, "OPENROUTER_API_KEY")
	if err != nil {
		return "", err
	}
	if envKey == "" {
		return "OPENROUTER_API_KEY not set", nil
	}
	return "no usable OpenRouter credentials found", nil
}
