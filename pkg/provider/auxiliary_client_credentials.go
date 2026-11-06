package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Value of an optional pool attribute, nil when the row leaves it empty
func auxEntryAttr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// Extra attribute of a pool row with a default when absent
func auxEntryExtra(entry *PooledCredential, key string, def any) any {
	if v, ok := entry.Extra[key]; ok {
		return v
	}
	return def
}

// Nous provider state from the pool or the active profile auth.json, nil when not active with a token
func auxReadNousAuth(ctx context.Context) map[string]any {
	present, entry := auxSelectPoolEntryFn(ctx, "nous")
	if present {
		if entry == nil {
			return nil
		}
		return map[string]any{
			"access_token":       entry.AccessToken,
			"refresh_token":      auxEntryAttr(entry.RefreshToken),
			"agent_key":          auxEntryAttr(entry.AgentKey),
			"inference_base_url": auxPoolRuntimeBaseURL(ctx, entry, auxNousDefaultBaseURL),
			"portal_base_url":    auxEntryExtra(entry, "portal_base_url", nil),
			"client_id":          auxEntryExtra(entry, "client_id", nil),
			"scope":              auxEntryExtra(entry, "scope", nil),
			"token_type":         auxEntryExtra(entry, "token_type", "Bearer"),
			"source":             "pool",
		}
	}
	provider, err := auxReadNousAuthFile(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not read Nous auth: %v", err))
		return nil
	}
	return provider
}

// Nous state of an auth.json whose active provider is nous, nil when absent, inactive or tokenless
func auxReadNousAuthFile(ctx context.Context) (map[string]any, error) {
	path := auxAuthJSONPath(ctx)
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &parsed); err != nil {
		return nil, err
	}
	data, ok := parsed.(map[string]any)
	if !ok {
		return nil, &auxAttributeError{typeName: pyrepr.TypeName(parsed), attr: "get"}
	}
	if data["active_provider"] != "nous" {
		return nil, nil
	}
	providers, ok := data["providers"]
	if !ok {
		providers = map[string]any{}
	}
	pm, ok := providers.(map[string]any)
	if !ok {
		return nil, &auxAttributeError{typeName: pyrepr.TypeName(providers), attr: "get"}
	}
	nous, ok := pm["nous"]
	if !ok {
		nous = map[string]any{}
	}
	provider, ok := nous.(map[string]any)
	if !ok {
		return nil, &auxAttributeError{typeName: pyrepr.TypeName(nous), attr: "get"}
	}
	if !pyrepr.Truthy(provider["agent_key"]) && !pyrepr.Truthy(provider["access_token"]) {
		return nil, nil
	}
	return provider, nil
}

// Usable Nous inference JWT of stored auth state, agent key before access token, empty when neither
func auxNousAPIKey(provider map[string]any) string {
	for _, pair := range [][2]string{{"agent_key", "agent_key_expires_at"}, {"access_token", "expires_at"}} {
		token, ok := provider[pair[0]].(string)
		if !ok || strings.TrimSpace(token) == "" {
			continue
		}
		if NousInvokeJWTStatus(token, provider["scope"], provider[pair[1]], NousInvokeJWTMinTTL) == "" {
			return token
		}
	}
	return ""
}

// Agent key and access token state of a Nous pool row for the usability checks
func auxNousEntryState(entry *PooledCredential) map[string]any {
	return map[string]any{
		"agent_key": auxEntryAttr(entry.AgentKey), "agent_key_expires_at": auxEntryAttr(entry.AgentKeyExpiresAt),
		"access_token": auxEntryAttr(entry.AccessToken), "expires_at": auxEntryAttr(entry.ExpiresAt),
		"scope": auxEntryExtra(entry, "scope", nil),
	}
}

// Credential pool surface the Nous runtime read selects from and refreshes
type auxRefreshablePool interface {
	HasCredentials() bool
	Select(model string) (PooledCredential, bool)
	TryRefreshCurrent() (PooledCredential, bool)
}

// Nous pool loader seam, nil when the profile holds no Nous pool
var auxLoadNousPoolFn = func(ctx context.Context) auxRefreshablePool {
	if pool := LoadPool(modelsPoolEnv(ctx), "nous"); pool != nil {
		return pool
	}
	return nil
}

// Pool entry selector seam the tests swap for a present but exhausted pool
var auxSelectPoolEntryFn = auxSelectPoolEntry

// Nous key and base URL from the selected pool row, refreshed when the agent key is short lived or forced
func auxResolveNousPoolRuntimeAPI(ctx context.Context, forceRefresh bool) (string, string, bool) {
	var pool auxRefreshablePool
	if err := auxRecoverRaise(func() { pool = auxLoadNousPoolFn(ctx) }); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary Nous pool credential resolution failed: %v", err))
		return "", "", false
	}
	if pool == nil || !pool.HasCredentials() {
		return "", "", false
	}
	var entry PooledCredential
	var found bool
	if err := auxRecoverRaise(func() { entry, found = pool.Select("") }); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary Nous pool selection failed: %v", err))
		return "", "", false
	}
	if !found {
		return "", "", false
	}
	minTTL := time.Duration(auxNousMinKeyTTLSeconds()) * time.Second
	if forceRefresh || !nousAgentKeyIsUsable(auxNousEntryState(&entry), minTTL) {
		var refreshed PooledCredential
		var ok bool
		if err := auxRecoverRaise(func() { refreshed, ok = pool.TryRefreshCurrent() }); err != nil {
			slog.Debug(fmt.Sprintf("Auxiliary Nous pool refresh failed: %v", err))
			ok = false
		}
		if !ok {
			return "", "", false
		}
		entry = refreshed
	}
	apiKey := auxNousAPIKey(auxNousEntryState(&entry))
	baseURL := auxPoolRuntimeBaseURL(ctx, &entry, auxNousDefaultBaseURL)
	if apiKey == "" || baseURL == "" {
		return "", "", false
	}
	return apiKey, baseURL, true
}

// Fresh Nous runtime credentials from the pool, then the auth store with a JWT refresh
func auxResolveNousRuntimeAPI(ctx context.Context, forceRefresh bool, staleAccessToken string) (string, string, bool) {
	if key, base, ok := auxResolveNousPoolRuntimeAPI(ctx, forceRefresh); ok {
		return key, base, true
	}
	timeout := time.Duration(envFloat("ICEBATH_NOUS_TIMEOUT_SECONDS", 15) * float64(time.Second))
	var creds map[string]any
	err := auxRecoverRaise(func() {
		var resolveErr error
		creds, resolveErr = auxResolveNousRuntimeCredentialsFn(ctx, NousResolveOptions{Timeout: timeout, ForceRefresh: forceRefresh,
			StaleAccessToken: staleAccessToken})
		if resolveErr != nil {
			panic(resolveErr)
		}
	})
	if err != nil {
		RecordNousCredentialFailure(ctx, err)
		return "", "", false
	}
	ClearNousCredentialFailure()
	return auxCredsPair(creds)
}

// Auth store Nous resolver seam the tests swap for a failing or canned refresh
var auxResolveNousRuntimeCredentialsFn = func(ctx context.Context, opts NousResolveOptions) (map[string]any, error) {
	return resolveNousRuntimeCredentialsWith(modelsPoolEnv(ctx), opts)
}

// Key and slash trimmed base URL of a runtime credentials map, false when either is missing
func auxCredsPair(creds map[string]any) (string, string, bool) {
	apiKey := strings.TrimSpace(auxOrEmptyStr(creds["api_key"]))
	baseURL := strings.TrimRight(strings.TrimSpace(auxOrEmptyStr(creds["base_url"])), "/")
	if apiKey == "" || baseURL == "" {
		return "", "", false
	}
	return apiKey, baseURL, true
}

// Trimmed slashless form of a URL candidate
func auxTrimURL(v string) string {
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// Fresh xAI OAuth key and base URL from the pool, then the singleton auth store resolver
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
		slog.Debug(fmt.Sprintf("Auxiliary xAI OAuth pool credential resolution failed: %v", err))
	} else if apiKey != "" && baseURL != "" {
		return apiKey, baseURL, true
	}
	var creds map[string]any
	if err := auxRecoverRaise(func() {
		var resolveErr error
		creds, resolveErr = auxResolveXaiOAuthRuntimeCredentialsFn(ctx)
		if resolveErr != nil {
			panic(resolveErr)
		}
	}); err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary xAI OAuth runtime credential resolution failed: %v", err))
		return "", "", false
	}
	return auxCredsPair(creds)
}

// Key and validated base URL of the selected xAI OAuth pool row, scoped base URL env first
func auxXaiOAuthPoolPair(ctx context.Context) (string, string, error) {
	env := modelsPoolEnv(ctx)
	pool := LoadPool(env, "xai-oauth")
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

// Singleton xAI OAuth resolver seam, refreshing an expiring token as hermes does by default
var auxResolveXaiOAuthRuntimeCredentialsFn = func(ctx context.Context) (map[string]any, error) {
	return resolveXAIOAuthRuntimeCredentials(modelsPoolEnv(ctx), false, true, 0)
}

// Codex token and the base URL of the same authority, an override first, else the pool row route or auth.json
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
	return auxReadCodexSingletonToken(ctx), override, nil
}

// Wall clock seam of the Codex expiry check
var auxCodexNow = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Profile auth.json Codex access token with expired JWTs skipped, empty when unusable
func auxReadCodexSingletonToken(ctx context.Context) string {
	var data codexTokenData
	if err := auxRecoverRaise(func() {
		var readErr error
		data, readErr = readCodexTokens(modelsPoolEnv(ctx), true)
		if readErr != nil {
			panic(readErr)
		}
	}); err != nil {
		slog.Debug(fmt.Sprintf("Could not read Codex auth for auxiliary client: %v", err))
		return ""
	}
	accessToken, ok := data.tokens["access_token"].(string)
	if !ok || strings.TrimSpace(accessToken) == "" {
		return ""
	}
	if exp, ok := auxJWTExp(accessToken); ok && exp != 0 && auxCodexNow() > exp {
		slog.Debug(fmt.Sprintf("Codex access token expired (exp=%s), skipping", pyrepr.Float(exp)))
		return ""
	}
	return strings.TrimSpace(accessToken)
}

// Numeric exp claim of a JWT payload, false for a non JWT token or a non numeric claim
func auxJWTExp(token string) (float64, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, false
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return 0, false
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0, false
	}
	switch v := claims["exp"].(type) {
	case nil:
		return 0, true
	case float64:
		return v, true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// Wire client of the first healthy API key provider with credentials and a known auxiliary model
func auxResolveAPIKeyProvider(ctx context.Context) (AuxClient, string, error) {
	env := modelsPoolEnv(ctx)
	for _, pconfig := range canonicalRegistryRows(ctx) {
		providerID := pconfig.Name
		if pconfig.AuthType != AuthTypeAPIKey {
			continue
		}
		if auxIsProviderUnhealthy(ctx, providerID, "") {
			slog.Debug(fmt.Sprintf("Auxiliary api-key chain: %s is unhealthy, skipping", providerID))
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
		slog.Debug(fmt.Sprintf("Auxiliary text client: %s (%s)%s", name, model, via))
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
			out[k] = pyrepr.Str(v)
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
		if profile := getProviderProfile(ctx, provider); profile != nil && len(profile.Headers) > 0 {
			headers = copyStringMap(profile.Headers)
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

// Free only flag and OpenRouter model of the auxiliary config, the free default when unreadable
func auxOpenrouterSettings(ctx context.Context) (bool, string) {
	tree, err := auxLoadConfigReadonlyFn(ctx)
	if err != nil {
		return false, auxOpenRouterModel
	}
	freeOnly, _ := config.CfgGet(tree, "auxiliary", "free_only")
	model := auxOpenRouterModel
	if val, ok := config.CfgGet(tree, "auxiliary", "openrouter_model"); ok {
		if s, isStr := val.(string); isStr && strings.TrimSpace(s) != "" {
			model = strings.TrimSpace(s)
		}
	}
	return pyrepr.Truthy(freeOnly), model
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
	slog.Warn(fmt.Sprintf("Auxiliary client: PAID lane engaged for auxiliary task — OpenRouter fallback model %s is not "+
		"a :free SKU and may incur real spend. Set auxiliary.free_only: true to restrict auxiliary "+
		"fallbacks to free models, or auxiliary.openrouter_model to a :free model.", pyrepr.Repr(model)))
}

// OpenRouter client from an explicit key, the pool or OPENROUTER_API_KEY, honouring the free only policy
func auxTryOpenrouter(ctx context.Context, explicitAPIKey any, model, explicitBaseURL string) (AuxClient, string, error) {
	freeOnly, cfgModel := auxOpenrouterSettings(ctx)
	orModel := model
	if orModel == "" {
		orModel = cfgModel
	}
	if freeOnly && !auxIsFreeModel(orModel) {
		slog.Warn(fmt.Sprintf("Auxiliary client: auxiliary.free_only is enabled but the OpenRouter fallback model %s is "+
			"not a :free SKU — skipping the OpenRouter fallback. Set auxiliary.openrouter_model to a "+
			":free model (e.g. nvidia/nemotron-3-ultra-550b-a55b:free) or disable auxiliary.free_only.", pyrepr.Repr(orModel)))
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
			slog.Debug("Auxiliary client: OpenRouter via pool")
			headers, err := BuildORHeaders(ctx, nil)
			if err != nil {
				return nil, "", err
			}
			return auxCreateOpenAIClient(ctx, orKey, baseURL, auxOpenAIOptions{DefaultHeaders: headers}), orModel, nil
		}
		slog.Debug("Auxiliary client: OpenRouter pool exhausted, trying OPENROUTER_API_KEY")
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
	slog.Debug("Auxiliary client: OpenRouter")
	headers, err := BuildORHeaders(ctx, nil)
	if err != nil {
		return nil, "", err
	}
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
			"the request was skipped before provider availability checks", pyrepr.Repr(orModel)), nil
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

// Portal recommended auxiliary model lookup seam
var auxGetNousRecommendedAuxModelFn = func(ctx context.Context, vision, forceRefresh bool) string {
	return GetNousRecommendedAuxModel(ctx, vision, nil, "", forceRefresh)
}

// Seconds left on the shared Nous rate limit, false when not limited
var auxNousRateLimitRemainingFn = func(ctx context.Context, anonymous bool) (float64, bool) {
	guard := &NousRateGuard{Home: home.Dir(ctx), Anonymous: anonymous}
	remaining, ok := guard.Remaining()
	return remaining.Seconds(), ok
}

// Nous Portal client from fresh runtime credentials or the stored JWT, nil when unauthenticated or rate limited
func auxTryNous(ctx context.Context, vision bool) (AuxClient, string, error) {
	nous := auxReadNousAuth(ctx)
	apiKey, baseURL, haveRuntime := auxResolveNousRuntimeAPI(ctx, false, "")
	if !haveRuntime && nous == nil {
		slog.Warn("Auxiliary Nous client unavailable: no Nous authentication found (run: icebath auth).")
		auxMarkProviderUnhealthy(ctx, "nous", 60, "", "no Nous authentication found", slog.LevelDebug)
		return nil, "", nil
	}
	if !haveRuntime {
		slog.Debug("Auxiliary Nous: runtime JWT refresh failed; checking stored auth.json token.")
		apiKey = auxNousAPIKey(nous)
		if apiKey == "" {
			slog.Warn("Auxiliary Nous client unavailable: no usable inference JWT found " +
				"(run: icebath auth add nous).")
			auxMarkProviderUnhealthy(ctx, "nous", 60, "", "no usable Nous inference JWT", slog.LevelDebug)
			return nil, "", nil
		}
		base := nous["inference_base_url"]
		if !pyrepr.Truthy(base) {
			envBase, err := auxScopedKeyEnv(ctx, "NOUS_INFERENCE_BASE_URL")
			if err != nil {
				return nil, "", err
			}
			base = envBase
		}
		if !pyrepr.Truthy(base) {
			base = auxNousDefaultBaseURL
		}
		baseURL = strings.TrimRight(pyrepr.Str(base), "/")
	}
	limited := false
	_ = auxRecoverRaise(func() {
		anonymous := IsAnonymousRequest("nous", apiKey)
		remaining, ok := auxNousRateLimitRemainingFn(ctx, anonymous)
		if ok && remaining > 0 {
			slog.Debug(fmt.Sprintf("Auxiliary: skipping Nous Portal (rate-limited, resets in %.0fs)", remaining))
			ttl := remaining
			if anonymous {
				ttl = math.Min(remaining, 60.0)
			}
			auxMarkProviderUnhealthy(ctx, "nous", ttl, "", "Nous Portal rate-limited", slog.LevelInfo)
			limited = true
		}
	})
	if limited {
		return nil, "", nil
	}
	lane := "text"
	if vision {
		lane = "vision"
	}
	if RouteIsWelcomeHost(baseURL) {
		slog.Debug(fmt.Sprintf("Auxiliary/%s: Nous free tier; using %s", lane, GuestModel))
		return auxCreateOpenAIClient(ctx, apiKey, baseURL, auxOpenAIOptions{}), GuestModel, nil
	}
	slog.Debug("Auxiliary client: Nous Portal")
	model := auxNousModel
	if !auxProbeActive(ctx) {
		var recommended string
		if err := auxRecoverRaise(func() { recommended = auxGetNousRecommendedAuxModelFn(ctx, vision, false) }); err != nil {
			slog.Debug(fmt.Sprintf("Auxiliary/%s: recommended-models lookup failed (%v); falling back to %s", lane, err, model))
		} else if recommended != "" {
			model = recommended
			slog.Debug(fmt.Sprintf("Auxiliary/%s: using Portal-recommended model %s", lane, model))
		} else {
			slog.Debug(fmt.Sprintf("Auxiliary/%s: no Portal recommendation, falling back to %s", lane, model))
		}
	}
	return auxCreateOpenAIClient(ctx, apiKey, baseURL, auxOpenAIOptions{}), model, nil
}

// Fresh Portal recommendation after a stale model 404, else the default, whichever differs from the stale model
func auxRefreshNousRecommendedModel(ctx context.Context, vision bool, staleModel string) string {
	stale := strings.ToLower(strings.TrimSpace(staleModel))
	fresh := ""
	if err := auxRecoverRaise(func() { fresh = auxGetNousRecommendedAuxModelFn(ctx, vision, true) }); err != nil {
		slog.Debug(fmt.Sprintf("Nous recommended-model refresh failed (%v); using default %s", err, auxNousModel))
	}
	if fresh != "" && strings.ToLower(strings.TrimSpace(fresh)) != stale {
		return fresh
	}
	if strings.ToLower(strings.TrimSpace(auxNousModel)) != stale {
		return auxNousModel
	}
	return ""
}
