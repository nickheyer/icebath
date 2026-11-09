package provider

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Secret safe blake2b over the selected entry key, else every pool entry key, empty when no key exists
func auxPoolCredentialDigest(pool *Pool, entry *PooledCredential) string {
	var keys []string
	if entry != nil {
		if k := auxPoolRuntimeAPIKey(entry); k != "" {
			keys = []string{k}
		}
	}
	if len(keys) == 0 {
		var entries []PooledCredential
		if pool != nil {
			if err := auxRecoverRaise(func() { entries = pool.Entries() }); err != nil {
				slog.Debug(fmt.Sprintf("Auxiliary client: could not list pool entries for digest: %v", err))
				return ""
			}
		}
		for i := range entries {
			if k := auxPoolRuntimeAPIKey(&entries[i]); k != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
	}
	if len(keys) == 0 {
		return ""
	}
	var buf []byte
	for _, key := range keys {
		buf = append(buf, key...)
		buf = append(buf, 0)
	}
	return hex.EncodeToString(blake2bSum(buf, 16))
}

// Cache discriminator of provider, peeked entry id and key digest so a rotated credential yields a new client
func auxPoolCacheHint(ctx context.Context, provider string, mainRuntime map[string]any) string {
	normalized := auxNormalizeAuxProvider(ctx, provider)
	if normalized == "auto" {
		runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
		rtProvider, _ := runtime["provider"].(string)
		if rtProvider == "" {
			rtProvider = auxReadMainProvider(ctx)
		}
		normalized = auxNormalizeAuxProvider(ctx, rtProvider)
	}
	if normalized == "" || normalized == "auto" || normalized == "custom" {
		return ""
	}
	pool := auxLoadPoolWithCredentials(ctx, normalized, " (cache hint)")
	if pool == nil {
		return ""
	}
	entry := auxPeekPoolEntry(ctx, normalized, pool)
	digest := auxPoolCredentialDigest(pool, entry)
	entryID := ""
	if entry != nil {
		entryID = pystr.Strip(entry.ID)
	}
	if entryID == "" && digest == "" {
		return ""
	}
	return normalized + ":" + entryID + ":" + digest
}

// Provider pool able to recover the client, empty when the session key went to a foreign origin or nothing matches
func auxRecoverablePoolProvider(ctx context.Context, resolvedProvider string, client AuxClient, mainRuntime map[string]any) (string, error) {
	normalized := auxNormalizeAuxProvider(ctx, resolvedProvider)
	base, clientKey := "", ""
	if client != nil {
		base, clientKey = client.BaseURL(), client.APIKey()
	}
	runtime := auxNormalizeMainRuntime(ctx, mainRuntime)
	rtBase := auxPyStr(runtime["base_url"])
	if !auxTruthy(runtime["base_url"]) {
		rtBase = ""
	}
	rtProvider, rtProviderIsStr := runtime["provider"].(string)
	rtKey, rtKeyIsStr := runtime["api_key"].(string)
	if base != "" && rtBase != "" && rtProviderIsStr && normalized == rtProvider && rtKeyIsStr && rtKey != "" && clientKey == rtKey {
		baseOrigin, err := auxBaseURLOrigin(base)
		if err != nil {
			return "", err
		}
		rtOrigin, err := auxBaseURLOrigin(rtBase)
		if err != nil {
			return "", err
		}
		if baseOrigin != rtOrigin {
			slog.Info(fmt.Sprintf("Auxiliary: %s rejected the session key at %s, but the session's endpoint is %s — endpoint mismatch, not a dead key; skipping credential rotation",
				normalized, baseURLHostname(base), baseURLHostname(rtBase)))
			return "", nil
		}
	}
	if normalized != "" && normalized != "auto" && normalized != "custom" {
		if pinned := pystr.Lower(pystr.Strip(resolvedProvider)); strings.HasPrefix(pinned, "custom:") {
			return pinned, nil
		}
		return normalized, nil
	}
	if known := auxProviderForHost(ctx, base, auxPoolProviderByHost); known != "" {
		return known, nil
	}
	if len(mainRuntime) == 0 || rtProvider == "" || rtProvider == "auto" || rtProvider == "custom" {
		return "", nil
	}
	var matched string
	_ = auxRecoverRaise(func() {
		pconfig := authRegistryGet(ctx, rtProvider)
		if pconfig == nil || pconfig.AuthType != AuthTypeAPIKey {
			return
		}
		poolBase := rtBase
		if poolBase == "" {
			poolBase = pconfig.BaseURL
		}
		poolBase = strings.TrimRight(poolBase, "/")
		if poolBase != "" && baseURLHostMatches(base, baseURLHostname(poolBase)) {
			matched = rtProvider
		}
	})
	return matched, nil
}

// Pool surface the same provider recovery drives
type auxRecoveryPool interface {
	HasCredentials() bool
	TryRefreshCurrent() (PooledCredential, bool)
	MarkExhaustedAndRotate(req RotationRequest) (PooledCredential, bool)
}

// Recovery pool seam, the entry scoped view for a pinned custom name and the whole pool otherwise
var auxRecoveryPoolFn = func(ctx context.Context, provider, normalized string) (auxRecoveryPool, error) {
	var pool *Pool
	err := auxRecoverRaise(func() {
		var cfg map[string]any
		if strings.HasPrefix(pystr.Lower(pystr.Strip(provider)), "custom:") {
			tree, err := auxLoadConfigFn(ctx)
			if err != nil {
				panic(err)
			}
			cfg = tree
		}
		pool = PoolForAuxProvider(provider, normalized, cfg, func(key string) *Pool { return auxLoadProviderPoolFn(ctx, key) })
	})
	if err != nil || pool == nil {
		return nil, err
	}
	return pool, nil
}

// Same provider pool recovery by refreshing or rotating the failed entry, true when a fresh credential exists
func auxRecoverProviderPool(ctx context.Context, provider string, exc error, failedAPIKey string) bool {
	normalized := auxNormalizeAuxProvider(ctx, provider)
	pool, loadErr := auxRecoveryPoolFn(ctx, provider, normalized)
	if loadErr != nil {
		slog.Debug(fmt.Sprintf("Auxiliary client: could not load pool for %s recovery: %v", normalized, loadErr))
		return false
	}
	if pool == nil || !pool.HasCredentials() {
		return false
	}
	status, hasStatus := auxExcStatusAttr(exc)
	rotate := func(fallbackStatus int) bool {
		errorContext := map[string]any{"message": exc.Error()}
		code := fallbackStatus
		if hasStatus {
			errorContext["status_code"] = status
			code = status
		}
		if _, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: code, ErrorContext: errorContext, APIKeyHint: failedAPIKey}); !ok {
			return false
		}
		auxEvictCachedClients(ctx, normalized)
		return true
	}
	if auxIsAuthError(exc) {
		if _, ok := pool.TryRefreshCurrent(); ok {
			auxEvictCachedClients(ctx, normalized)
			return true
		}
		return rotate(401)
	}
	if auxIsPaymentError(exc) {
		return rotate(402)
	}
	if auxIsRateLimitError(exc) && !auxIsOverloadedError(exc) {
		return rotate(429)
	}
	return false
}

// Request arguments a same provider retry rebuilds its client and kwargs from after credential recovery
type auxSameProviderRetry struct {
	Task               string
	ResolvedProvider   string
	ResolvedModel      string
	ResolvedBaseURL    string
	ResolvedAPIKey     any
	ResolvedAPIMode    string
	MainRuntime        map[string]any
	FinalModel         string
	Messages           any
	Temperature        *float64
	MaxTokens          any
	Tools              any
	EffectiveTimeout   float64
	EffectiveExtraBody map[string]any
	ReasoningConfig    map[string]any
	ExtraHeaders       map[string]string
}

// Rebuilt client and request kwargs for a same provider retry, an error when the provider cannot be rebuilt
func auxPrepareSameProviderRetry(ctx context.Context, prep auxSameProviderRetry, asyncMode bool) (AuxClient, map[string]any, error) {
	var retryClient AuxClient
	var retryModel, effectiveProvider string
	if prep.Task == "vision" {
		var err error
		effectiveProvider, retryClient, retryModel, err = auxResolveVisionProviderClientFn(ctx, prep.ResolvedProvider, prep.FinalModel, prep.ResolvedBaseURL, prep.ResolvedAPIKey, asyncMode, nil)
		if err != nil {
			return nil, nil, err
		}
	} else {
		var err error
		retryClient, retryModel, err = auxGetCachedClientFn(ctx, auxCachedClientRequest{
			Provider: prep.ResolvedProvider, Model: prep.ResolvedModel, AsyncMode: asyncMode, BaseURL: prep.ResolvedBaseURL,
			APIKey: prep.ResolvedAPIKey, APIMode: prep.ResolvedAPIMode, MainRuntime: prep.MainRuntime,
		})
		if err != nil {
			return nil, nil, err
		}
		effectiveProvider = auxEffectiveProviderForClient(retryClient, prep.ResolvedProvider)
	}
	if retryClient == nil {
		task := prep.Task
		if task == "" {
			task = "call"
		}
		return nil, nil, fmt.Errorf("Auxiliary %s: provider %s could not be rebuilt after recovery", task, prep.ResolvedProvider)
	}
	retryBase := retryClient.BaseURL()
	provider := effectiveProvider
	if provider == "" {
		provider = prep.ResolvedProvider
	}
	model := retryModel
	if model == "" {
		model = prep.FinalModel
	}
	base := retryBase
	if base == "" {
		base = prep.ResolvedBaseURL
	}
	retryKwargs, err := auxBuildCallKwargs(ctx, provider, model, prep.Messages, auxCallKwargsOptions{
		Temperature: prep.Temperature, MaxTokens: prep.MaxTokens, Tools: prep.Tools, Timeout: prep.EffectiveTimeout,
		ExtraBody: prep.EffectiveExtraBody, ReasoningConfig: prep.ReasoningConfig, BaseURL: base, Task: prep.Task,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(prep.ExtraHeaders) > 0 {
		headers := make(map[string]string, len(prep.ExtraHeaders))
		for k, v := range prep.ExtraHeaders {
			headers[k] = v
		}
		retryKwargs["extra_headers"] = headers
	}
	if auxIsAnthropicCompatEndpoint(prep.ResolvedProvider, retryBase) {
		retryKwargs["messages"] = auxConvertOpenAIImagesToAnthropic(retryKwargs["messages"])
	}
	return retryClient, retryKwargs, nil
}

// Validated response of a rebuilt client retry through the relayed sync completion
func auxRetrySameProviderSync(ctx context.Context, prep auxSameProviderRetry) (any, error) {
	client, kwargs, err := auxPrepareSameProviderRetry(ctx, prep, false)
	if err != nil {
		return nil, err
	}
	out, err := auxRelaySyncCompletionFn(ctx, client, kwargs, prep.ResolvedProvider, prep.ResolvedAPIMode, nil)
	if err != nil {
		return nil, err
	}
	return auxValidateLLMResponseFn(ctx, out, prep.Task, "", "")
}

// Validated response of a rebuilt async client retry through the relayed async completion
func auxRetrySameProviderAsync(ctx context.Context, prep auxSameProviderRetry) (any, error) {
	client, kwargs, err := auxPrepareSameProviderRetry(ctx, prep, true)
	if err != nil {
		return nil, err
	}
	out, err := auxRelayAsyncCompletion(ctx, client, kwargs, prep.ResolvedProvider, prep.ResolvedAPIMode, nil)
	if err != nil {
		return nil, err
	}
	return auxValidateLLMResponseFn(ctx, out, prep.Task, "", "")
}

// Codex runtime resolver seam forcing a token refresh
var auxResolveCodexRuntimeCredentialsFn = func(ctx context.Context, forceRefresh bool) (map[string]any, error) {
	return resolveCodexRuntimeCredentials(modelsPoolEnv(ctx), CodexResolveOptions{ForceRefresh: forceRefresh})
}

// Forced xAI OAuth singleton resolver seam used by the refresh path
var auxResolveXaiOAuthForceRefreshFn = func(ctx context.Context) (map[string]any, error) {
	return resolveXAIOAuthRuntimeCredentials(modelsPoolEnv(ctx), true, true, 0)
}

// Anthropic credential seams over the pool, the Claude Code login and its OAuth refresh
var (
	auxLoadProviderPoolFn          = func(ctx context.Context, provider string) *Pool { return LoadPool(modelsPoolEnv(ctx), provider) }
	auxReadClaudeCodeCredentialsFn = func(ctx context.Context) map[string]any { return ReadClaudeCodeCredentials(modelsPoolEnv(ctx)) }
	auxRefreshClaudeOAuthTokenFn   = func(ctx context.Context, creds map[string]any) string {
		return refreshClaudeCodeOAuthToken(modelsPoolEnv(ctx), creds)
	}
	auxVertexConfigFn = func(ctx context.Context) (string, string) { return vertexGetConfig(ctx, vertexHomeEnv(ctx), "", "") }
)

// True when a forced Codex refresh yields an api key
func auxRefreshCodexCredentials(ctx context.Context, _ string) (bool, error) {
	creds, err := auxResolveCodexRuntimeCredentialsFn(ctx, true)
	if err != nil {
		return false, err
	}
	return auxCredsHaveAPIKey(creds), nil
}

// True when a forced Nous refresh yields an api key
func auxRefreshNousCredentials(ctx context.Context, _ string) (bool, error) {
	timeout := envFloat("ICEBATH_NOUS_TIMEOUT_SECONDS", 15)
	creds, err := auxResolveNousRuntimeCredentialsFn(ctx, NousResolveOptions{Timeout: secondsDuration(timeout), ForceRefresh: true})
	if err != nil {
		return false, err
	}
	return auxCredsHaveAPIKey(creds), nil
}

// Refreshes only the failed Anthropic key, through its pool entry or the matching Claude Code login
func auxRefreshAnthropicCredentials(ctx context.Context, failedAPIKey string) (bool, error) {
	if failedAPIKey == "" {
		return false, nil
	}
	pool := auxLoadProviderPoolFn(ctx, "anthropic")
	if pool != nil && pool.EntryIDForAPIKey(failedAPIKey) != "" {
		_, ok := pool.TryRefreshMatching(failedAPIKey, "")
		return ok, nil
	}
	creds := auxReadClaudeCodeCredentialsFn(ctx)
	if creds != nil && creds["accessToken"] == failedAPIKey && auxTruthy(creds["refreshToken"]) {
		return auxRefreshClaudeOAuthTokenFn(ctx, creds) != "", nil
	}
	return false, nil
}

// Pool level xAI OAuth refresh first, then the forced singleton auth store resolver
func auxRefreshXaiOAuthCredentials(ctx context.Context, _ string) (bool, error) {
	pool := auxLoadProviderPoolFn(ctx, "xai-oauth")
	if pool != nil && pool.HasCredentials() {
		pool.Select("")
		if refreshed, ok := pool.TryRefreshCurrent(); ok && pystr.Strip(refreshed.RuntimeAPIKey()) != "" {
			return true, nil
		}
	}
	creds, err := auxResolveXaiOAuthForceRefreshFn(ctx)
	if err != nil {
		return false, err
	}
	return auxCredsHaveAPIKey(creds), nil
}

// True when a Vertex bearer and base URL can be minted
func auxRefreshVertexCredentials(ctx context.Context, _ string) (bool, error) {
	token, baseURL := auxVertexConfigFn(ctx)
	return pystr.Strip(token) != "" && pystr.Strip(baseURL) != "", nil
}

// Refreshers keyed by provider, each true when a usable credential exists afterwards
var auxCredentialRefreshers = map[string]func(ctx context.Context, failedAPIKey string) (bool, error){
	"openai-codex": auxRefreshCodexCredentials,
	"nous":         auxRefreshNousCredentials,
	"anthropic":    auxRefreshAnthropicCredentials,
	"xai-oauth":    auxRefreshXaiOAuthCredentials,
	"vertex":       auxRefreshVertexCredentials,
}

// Refreshes short lived OAuth credentials or a profile runtime credential and evicts cached clients on success
func auxRefreshProviderCredentials(ctx context.Context, provider, failedAPIKey string) bool {
	normalized := auxNormalizeAuxProvider(ctx, provider)
	refresher, ok := auxCredentialRefreshers[normalized]
	if !ok {
		profile, found := GetProviderProfile(ctx, normalized)
		if !found {
			return false
		}
		refresher = func(ctx context.Context, _ string) (bool, error) {
			_, _, ok, err := profile.RefreshRuntimeCredentials(ctx, "auxiliary_unauthorized")
			return ok, err
		}
	}
	var refreshed bool
	err := auxRecoverRaise(func() {
		var rerr error
		if refreshed, rerr = refresher(ctx, failedAPIKey); rerr != nil {
			panic(rerr)
		}
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("Auxiliary provider credential refresh failed for %s: %v", normalized, err))
		return false
	}
	if !refreshed {
		return false
	}
	auxEvictCachedClients(ctx, normalized)
	return true
}

// Provider whose short lived credentials refresh, the base URL host deciding for auto routes
func auxAuthRefreshProviderForRoute(ctx context.Context, resolvedProvider, clientBaseURL, effectiveProvider string) string {
	normalized := auxNormalizeAuxProvider(ctx, resolvedProvider)
	if normalized != "" && normalized != "auto" {
		return normalized
	}
	hostProvider := auxProviderForHost(ctx, clientBaseURL, auxAuthRefreshProviderByHost)
	if hostProvider != "" && hostProvider == auxNormalizeAuxProvider(ctx, effectiveProvider)+"-oauth" {
		return normalized
	}
	if hostProvider != "" {
		return hostProvider
	}
	return normalized
}

var auxFallbackChainLabelRe = regexp.MustCompile(`^fallback_chain\[([0-9]+)\]`)

// Configured fallback chain entry a fallback chain label points at, nil when the label or index does not resolve
func auxFallbackChainEntry(ctx context.Context, task, fbLabel string) map[string]any {
	if task == "" || fbLabel == "" {
		return nil
	}
	m := auxFallbackChainLabelRe.FindStringSubmatch(fbLabel)
	if m == nil {
		return nil
	}
	cfg, err := auxGetAuxiliaryTaskConfig(ctx, task)
	if err != nil {
		return nil
	}
	chain, ok := cfg["fallback_chain"].([]any)
	if !ok {
		return nil
	}
	idx, err := strconv.Atoi(m[1])
	if err != nil || idx >= len(chain) {
		return nil
	}
	entry, _ := chain[idx].(map[string]any)
	return entry
}
