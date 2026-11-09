package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ---- resolve_nous_access_token ----

// Per process memo collapsing a burst of managed tool checks into one Portal round trip, keyed by home
var (
	resolveTokenCacheMu sync.Mutex
	resolveTokenCache   = map[string]resolvedNousToken{}
)

type resolvedNousToken struct {
	at    time.Time
	token string
}

const resolveTokenCacheTTL = 5 * time.Second

// NousAccessTokenOptions tune the refresh aware Portal token resolution
type NousAccessTokenOptions struct {
	Timeout     time.Duration
	Insecure    *bool
	CABundle    string
	RefreshSkew time.Duration
}

func (o NousAccessTokenOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 15 * time.Second
	}
	return o.Timeout
}

func (o NousAccessTokenOptions) refreshSkew() time.Duration {
	if o.RefreshSkew <= 0 {
		return AccessTokenRefreshSkew
	}
	return o.RefreshSkew
}

// ResolveNousAccessToken is the refresh aware Portal bearer for managed tools, connectors, and account reads
func ResolveNousAccessToken(env PoolEnv, opts NousAccessTokenOptions) (string, error) {
	return runWithChallenge(env, func() (string, error) { return resolveNousAccessToken(env, opts) })
}

// Memo key for the active store so a multiplexed sibling profile never receives this profile's bearer
func resolveTokenMemoKey(env PoolEnv) string {
	if env.Store == nil {
		return homeKey(home.Process())
	}
	return homeKey(env.Store.Home())
}

func resolveNousAccessToken(env PoolEnv, opts NousAccessTokenOptions) (string, error) {
	memoable := (opts.Insecure == nil || !*opts.Insecure) && opts.CABundle == ""
	cacheKey := resolveTokenMemoKey(env)
	if memoable {
		resolveTokenCacheMu.Lock()
		cached, ok := resolveTokenCache[cacheKey]
		resolveTokenCacheMu.Unlock()
		if ok && time.Since(cached.at) < resolveTokenCacheTTL {
			return cached.token, nil
		}
	}
	memo := func(token string) string {
		if memoable {
			resolveTokenCacheMu.Lock()
			resolveTokenCache[cacheKey] = resolvedNousToken{at: time.Now(), token: token}
			resolveTokenCacheMu.Unlock()
		}
		return token
	}
	timeout := opts.timeout()
	skew := opts.refreshSkew()
	result := ""
	err := env.Store.ProviderTransaction("nous", AuthLockTimeout, func(tx *ProviderTxn) error {
		state := tx.State
		if len(state) == 0 {
			return nousErr("Icebath is not logged into Nous Portal.", "nous_auth_missing", true)
		}
		portalBaseURL := nousPortalBaseURL(state)
		clientID := anyString(state["client_id"])
		if clientID == "" {
			clientID = DefaultNousClientID
		}
		verify := resolveOAuthVerify(env, opts.Insecure, opts.CABundle, state)
		persist := func() error { return tx.SaveState(state) }
		lockTimeout := timeout + 5*time.Second
		if lockTimeout < AuthLockTimeout {
			lockTimeout = AuthLockTimeout
		}
		unlock, err := nousSharedStoreLock(env, lockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		if isGuestState(state) {
			if accessToken := heldConnectorsToken(state, skew); accessToken != "" {
				result = memo(accessToken)
				return nil
			}
			client := oauthHTTPClient(env, timeout, verify)
			minted, err := refreshGuestState(env, state, client, "connectors")
			if err != nil {
				return err
			}
			if err := persist(); err != nil {
				return err
			}
			writeSharedNousState(env, state, true)
			result = memo(minted)
			return nil
		}
		mergedShared := mergeSharedNousOAuthState(env, state)
		accessToken, _ := state["access_token"].(string)
		refreshToken, _ := state["refresh_token"].(string)
		if accessToken == "" {
			return nousErr("No access token found for Nous Portal login.", "nous_auth_missing_access_token", true)
		}
		if !IsExpiring(state["expires_at"], skew) {
			if mergedShared {
				if err := persist(); err != nil {
					return err
				}
			}
			result = memo(accessToken)
			return nil
		}
		if refreshToken == "" {
			return nousErr("Session expired and no refresh token is available.", "nous_auth_missing_refresh_token", true)
		}
		client := oauthHTTPClient(env, timeout, verify)
		refreshed, err := refreshNousOrQuarantine(env, client, tx, state, portalBaseURL, clientID, refreshToken, "managed_access_token_refresh_failure", persist)
		if err != nil {
			return err
		}
		applyNousRefreshedTokens(state, refreshed, refreshToken, "")
		state["portal_base_url"] = portalBaseURL
		state["client_id"] = clientID
		state["tls"] = tlsStateFromVerify(verify.Insecure, verify.CABundle)
		if err := persist(); err != nil {
			return err
		}
		writeSharedNousState(env, state, true)
		result = memo(anyString(state["access_token"]))
		return nil
	})
	if err != nil {
		return "", err
	}
	return result, nil
}

// ---- status helpers ----

const nousAuthStatusCacheTTL = 15 * time.Second

type nousAuthStatusEntry struct {
	at         time.Time
	fileKey    string
	version    string
	hasVersion bool
	status     map[string]any
}

var (
	nousAuthStatusMu    sync.Mutex
	nousAuthStatusCache *nousAuthStatusEntry
)

// Uncached Nous status builder the memo wraps, swappable so tests can count computations
var computeNousAuthStatusFn = computeNousAuthStatus

// Symlink resolved absolute form of a store path
func authStoreKey(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// Active store identity and file version, the version absent for a missing file
func authFileCacheKey(env PoolEnv) (string, string, bool) {
	authFile := env.Store.Path()
	version, err := fileCacheVersion(authFile)
	if err != nil {
		return authStoreKey(authFile), "", false
	}
	return authStoreKey(authFile), version, true
}

// InvalidateNousAuthStatusCache clears the Nous status memo for writers that bypass auth.json
func InvalidateNousAuthStatusCache() {
	nousAuthStatusMu.Lock()
	defer nousAuthStatusMu.Unlock()
	nousAuthStatusCache = nil
}

// GetNousAuthStatus is the Nous status snapshot memoised fifteen seconds per auth.json version
func GetNousAuthStatus(env PoolEnv) map[string]any {
	now := time.Now()
	fileKey, version, hasVersion := authFileCacheKey(env)
	nousAuthStatusMu.Lock()
	cached := nousAuthStatusCache
	nousAuthStatusMu.Unlock()
	if cached != nil && cached.fileKey == fileKey && cached.version == version && cached.hasVersion == hasVersion && now.Sub(cached.at) < nousAuthStatusCacheTTL {
		return copyStatus(cached.status)
	}
	status := computeNousAuthStatusFn(env)
	nousAuthStatusMu.Lock()
	nousAuthStatusCache = &nousAuthStatusEntry{at: now, fileKey: fileKey, version: version, hasVersion: hasVersion, status: copyStatus(status)}
	nousAuthStatusMu.Unlock()
	return status
}

func copyStatus(status map[string]any) map[string]any {
	out := make(map[string]any, len(status))
	for k, v := range status {
		out[k] = v
	}
	return out
}

// OAuthResolveOptions are the per call knobs an OAuth runtime credential resolver accepts
type OAuthResolveOptions struct {
	ForceRefresh        bool
	SkipExpiringRefresh bool
	ReadOnly            bool
	RefreshSkew         time.Duration
	StaleAccessToken    string
}

// OAuthProviderFlow is the per provider OAuth plumbing keyed by provider id in OAuthProviderFlows
type OAuthProviderFlow struct {
	ProviderID string
	Resolve    func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error)
	Status     func(env PoolEnv) map[string]any
	// Codes for which retrying the same refresh token cannot succeed
	TerminalRefreshCodes map[string]bool
	// Logout with no active provider falls back to config.yaml model.provider for these
	LogoutFromConfig bool
}

// IsTerminalRefreshError is true when the error is this provider's relogin required grant death
func (f OAuthProviderFlow) IsTerminalRefreshError(err error) bool {
	var authErr *AuthError
	if !errors.As(err, &authErr) {
		return false
	}
	return authErr.Provider == f.ProviderID && f.TerminalRefreshCodes[authErr.Code] && authErr.ReloginRequired
}

// Nous state shape failures raised before any refresh POST that a retry cannot fix either
var nousAuthMissingCodes = map[string]bool{
	"nous_auth_missing": true, "nous_auth_missing_access_token": true, "nous_auth_missing_refresh_token": true,
}

func codeSet(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, set := range sets {
		for k := range set {
			out[k] = true
		}
	}
	return out
}

// OAuthProviderFlows is the registry of built in OAuth providers, filled at init so the pool tables can name its predicates
var OAuthProviderFlows map[string]OAuthProviderFlow

func init() {
	OAuthProviderFlows = builtinOAuthProviderFlows()
}

func builtinOAuthProviderFlows() map[string]OAuthProviderFlow {
	return map[string]OAuthProviderFlow{
		"nous": {
			ProviderID: "nous",
			Resolve: func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error) {
				return resolveNousRuntimeCredentials(env, opts.ForceRefresh, opts.StaleAccessToken)
			},
			Status:               GetNousAuthStatus,
			TerminalRefreshCodes: codeSet(oauthGrantDeadCodes, nousAuthMissingCodes),
			LogoutFromConfig:     true,
		},
		"openai-codex": {
			ProviderID: "openai-codex",
			Resolve: func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error) {
				return resolveCodexRuntimeCredentials(env, CodexResolveOptions{
					ForceRefresh: opts.ForceRefresh, SkipExpiringRefresh: opts.SkipExpiringRefresh,
					RefreshSkew: opts.RefreshSkew, ReadOnly: opts.ReadOnly})
			},
			Status:               GetCodexAuthStatus,
			TerminalRefreshCodes: codeSet(oauthGrantDeadCodes, map[string]bool{"codex_refresh_failed": true, "codex_auth_missing_refresh_token": true}),
			LogoutFromConfig:     true,
		},
		"xai-oauth": {
			ProviderID: "xai-oauth",
			Resolve: func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error) {
				return resolveXAIOAuthRuntimeCredentials(env, opts.ForceRefresh, !opts.SkipExpiringRefresh, opts.RefreshSkew)
			},
			Status:               GetXAIOAuthAuthStatus,
			TerminalRefreshCodes: map[string]bool{"xai_refresh_failed": true, "xai_auth_missing_refresh_token": true},
			LogoutFromConfig:     true,
		},
		"qwen-oauth": {
			ProviderID: "qwen-oauth",
			Resolve: func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error) {
				return resolveQwenRuntimeCredentials(env, !opts.SkipExpiringRefresh)
			},
			Status: getQwenAuthStatus,
		},
		"minimax-oauth": {
			ProviderID: "minimax-oauth",
			Resolve: func(env PoolEnv, opts OAuthResolveOptions) (map[string]any, error) {
				return resolveMinimaxOAuthRuntimeCredentials(env)
			},
			Status: GetMinimaxOAuthAuthStatus,
		},
	}
}

// True when retrying the same provider refresh token cannot succeed
func isTerminalRefreshError(err error, provider string) bool {
	return OAuthProviderFlows[provider].IsTerminalRefreshError(err)
}

func isTerminalNousRefreshError(err error) bool { return isTerminalRefreshError(err, "nous") }

func isTerminalXAIOAuthRefreshError(err error) bool { return isTerminalRefreshError(err, "xai-oauth") }

func isTerminalCodexOAuthRefreshError(err error) bool {
	return isTerminalRefreshError(err, "openai-codex")
}

// Degraded Codex status for a pool only credential frozen by a quota cooldown
func codexPoolRateLimitedStatus(env PoolEnv) map[string]any {
	rateLimit := codexPoolRateLimitStatus(env)
	if rateLimit == nil {
		return nil
	}
	label := anyString(rateLimit["label"])
	if label == "" {
		label = "unknown"
	}
	message := anyString(rateLimit["message"])
	if message == "" {
		message = "Codex provider quota exhausted; retry after the usage limit resets."
	}
	return map[string]any{
		"logged_in": true, "auth_store": env.Store.Path(), "last_refresh": rateLimit["last_refresh"],
		"auth_mode": "chatgpt", "source": "pool:" + label, "rate_limited": true,
		"error_code": CodexRateLimitedCode, "error": message, "reset_at": rateLimit["reset_at"],
	}
}

// Status snapshot for a store backed OAuth provider, the pool observed first then the legacy state
func poolFirstOAuthStatus(env PoolEnv, providerID string, isExpiring func(string, time.Duration) bool, authMode string,
	resolve func() (map[string]any, error), onPoolMiss func() map[string]any) map[string]any {
	authStore := env.Store.Path()
	if pool := LoadPool(env, providerID); pool != nil && pool.HasCredentials() {
		if entry, ok := pool.Peek(); ok {
			apiKey := entry.RuntimeAPIKey()
			if apiKey == "" {
				apiKey = entry.AccessToken
			}
			if apiKey != "" && !isExpiring(apiKey, 0) {
				label := entry.Label
				if label == "" {
					label = "unknown"
				}
				baseURL := entry.RuntimeBaseURL()
				if baseURL == "" {
					baseURL = entry.BaseURL
				}
				var lastRefresh any
				if entry.LastRefresh != "" {
					lastRefresh = entry.LastRefresh
				}
				return map[string]any{
					"logged_in": true, "auth_store": authStore, "last_refresh": lastRefresh, "auth_mode": authMode,
					"source": "pool:" + label, "api_key": apiKey, "base_url": strings.TrimRight(baseURL, "/"),
				}
			}
		}
		if onPoolMiss != nil {
			if degraded := onPoolMiss(); degraded != nil {
				return degraded
			}
		}
	}
	creds, err := resolve()
	if err != nil {
		return map[string]any{"logged_in": false, "auth_store": authStore, "error": err.Error()}
	}
	return map[string]any{
		"logged_in": true, "auth_store": authStore, "last_refresh": creds["last_refresh"], "auth_mode": creds["auth_mode"],
		"source": creds["source"], "api_key": creds["api_key"], "base_url": anyString(creds["base_url"]),
	}
}

// GetCodexAuthStatus is the read only Codex status snapshot, pool first then the legacy provider state
func GetCodexAuthStatus(env PoolEnv) map[string]any {
	status := poolFirstOAuthStatus(env, "openai-codex", codexAccessTokenIsExpiring, "chatgpt",
		func() (map[string]any, error) { return resolveCodexRuntimeCredentialsReadOnly(env) },
		func() map[string]any { return codexPoolRateLimitedStatus(env) })
	if strings.HasPrefix(anyString(status["source"]), "pool:") {
		status["base_url"] = codexPoolRouteBaseURL(env, anyString(status["base_url"]))
	}
	return status
}

// GetXAIOAuthAuthStatus is the xAI OAuth status snapshot, device code being the only xAI flow
func GetXAIOAuthAuthStatus(env PoolEnv) map[string]any {
	return poolFirstOAuthStatus(env, "xai-oauth", xaiAccessTokenIsExpiring, "oauth_device_code",
		func() (map[string]any, error) { return resolveXAIOAuthRuntimeCredentials(env, false, false, 0) }, nil)
}

// Resolver over the env's home, config, and secrets for the status and credential helpers
func authResolver(env PoolEnv) *resolver {
	h := home.Process()
	if env.Store != nil {
		h = env.Store.Home()
	}
	creds := newHomeCredentials(env)
	return newResolver(env.ctx(), config.Tree(creds.env.config()), creds.env.Secrets, h, creds)
}

// GetAPIKeyProviderStatus is the status snapshot for an API key provider
func GetAPIKeyProviderStatus(env PoolEnv, providerID string) map[string]any {
	pconfig, ok := registryProfileCtx(env.ctx(), providerID)
	if !ok || pconfig.AuthType != AuthTypeAPIKey {
		return map[string]any{"configured": false}
	}
	r := authResolver(env)
	apiKey, keySource, _ := r.apiKeyProviderSecret(providerID, pconfig)
	envURL := r.providerEnvBaseURL(pconfig)
	var baseURL string
	if providerID == "kimi-coding" || providerID == "kimi-coding-cn" {
		baseURL = resolveKimiBaseURL(apiKey, pconfig.BaseURL, envURL)
	} else if envURL != "" {
		baseURL = envURL
	} else {
		baseURL = pconfig.BaseURL
	}
	actualLocalNoAuth := false
	if providerID == "actual" {
		baseURL = NormalizeActualBaseURL(baseURL)
		actualLocalNoAuth = apiKey == "" && isActualLocalBaseURL(baseURL)
	}
	configured := apiKey != "" || actualLocalNoAuth
	if keySource == "" && actualLocalNoAuth {
		keySource = "local-offline"
	}
	return map[string]any{
		"configured": configured, "provider": providerID, "name": pconfig.Label(), "key_source": keySource,
		"base_url": baseURL, "logged_in": configured,
	}
}

// Positive evidence that an external process CLI is authed, the binary resolving being the generic proof
func externalProcessAuthEvidence(ctx context.Context, providerID, resolvedCommand string) (bool, string) {
	if p := getProviderProfile(ctx, providerID); p != nil {
		if verified, source, ok := p.ExternalAuthEvidence(); ok {
			return verified, source
		}
	}
	if resolvedCommand != "" {
		return true, "command: " + resolvedCommand
	}
	return false, ""
}

// GetExternalProcessProviderStatus is the structural status of a provider that runs a local subprocess
func GetExternalProcessProviderStatus(env PoolEnv, providerID string) map[string]any {
	pconfig, ok := registryProfileCtx(env.ctx(), providerID)
	if !ok || pconfig.AuthType != AuthTypeExternalProcess {
		return map[string]any{"configured": false}
	}
	command, args, baseURL, resolvedCommand, _ := authResolver(env).externalProcessSpec(pconfig)
	available := resolvedCommand != "" || strings.HasPrefix(baseURL, "acp+tcp://")
	authVerified, authSource := externalProcessAuthEvidence(env.ctx(), providerID, resolvedCommand)
	var resolved, source any
	if resolvedCommand != "" {
		resolved = resolvedCommand
	}
	if authSource != "" {
		source = authSource
	}
	if args == nil {
		args = []string{}
	}
	return map[string]any{
		"configured": available, "provider": providerID, "name": pconfig.Label(), "command": command,
		"args": args, "resolved_command": resolved, "base_url": baseURL,
		"logged_in": available, "auth_verified": authVerified, "auth_source": source,
	}
}

// AWS SDK providers answer through the credential chain
func getAWSSDKAuthStatus(target string) map[string]any {
	return map[string]any{"logged_in": hasAWSCredentials(true), "provider": target}
}

// Bespoke status builders winning over the auth type keyed fallbacks
func bespokeStatusFunctions() map[string]func(env PoolEnv) map[string]any {
	out := map[string]func(env PoolEnv) map[string]any{}
	for pid, flow := range OAuthProviderFlows {
		out[pid] = flow.Status
	}
	out["azure-foundry"] = getAzureFoundryAuthStatus
	return out
}

var statusByAuthType = map[string]func(env PoolEnv, provider string) map[string]any{
	AuthTypeExternalProcess: GetExternalProcessProviderStatus,
	AuthTypeAPIKey:          GetAPIKeyProviderStatus,
	AuthTypeAWSSDK:          func(env PoolEnv, provider string) map[string]any { return getAWSSDKAuthStatus(provider) },
	AuthTypeOAuthDeviceCode: getPluginOAuthAuthStatus,
	AuthTypeOAuthExternal:   getPluginOAuthAuthStatus,
}

// GetAuthStatus dispatches to the bespoke builder, then the registry auth type, for the provider or the active one
func GetAuthStatus(env PoolEnv, providerID string) map[string]any {
	target := strings.ToLower(strings.TrimSpace(providerID))
	if target == "" {
		active, _ := env.Store.ActiveProvider()
		target = strings.ToLower(strings.TrimSpace(active))
	}
	if target == "" {
		return map[string]any{"logged_in": false}
	}
	if statusFn, ok := bespokeStatusFunctions()[target]; ok {
		return statusFn(env)
	}
	if pconfig, ok := registryProfileCtx(env.ctx(), target); ok {
		if byType, ok := statusByAuthType[pconfig.AuthType]; ok {
			return byType(env, target)
		}
	}
	return map[string]any{"logged_in": false}
}

// Structural Azure Foundry status, the Entra chain never invoked so the CLI starts flat
func getAzureFoundryAuthStatus(env PoolEnv) map[string]any {
	info := map[string]any{"provider": "azure-foundry"}
	cfg := env.config()
	modelCfg, _ := cfg["model"].(map[string]any)
	if modelCfg == nil {
		modelCfg = map[string]any{}
	}
	authMode := strings.ToLower(strings.TrimSpace(cfgString(modelCfg["auth_mode"])))
	if authMode == "" {
		authMode = "api_key"
	}
	info["auth_mode"] = authMode
	info["base_url"] = strings.TrimSpace(cfgString(modelCfg["base_url"]))
	if authMode == "entra_id" {
		entraCfg, _ := modelCfg["entra"].(map[string]any)
		scope := strings.TrimSpace(cfgString(entraCfg["scope"]))
		if scope == "" {
			scope = ScopeAIAzureDefault
		}
		info["azure_identity_installed"] = true
		info["scope"] = scope
		info["credential_probe"] = "not_run"
		info["credential_verified"] = false
		info["logged_in"] = true
		info["hint"] = "azure-identity is installed; live credential validation is skipped here. Run `icebath doctor` to verify token acquisition."
		return info
	}
	apiKey := GetEnvPreferDotenv(env, "AZURE_FOUNDRY_API_KEY")
	info["logged_in"] = hasUsableSecret(apiKey)
	return info
}

// ---- config helpers for login and logout ----

// Removes stale inline endpoint credentials from a model config
func clearModelEndpointCredentials(modelCfg map[string]any, clearAPIKey, clearAPIMode, clearBaseURL bool) map[string]any {
	if modelCfg == nil {
		return modelCfg
	}
	if clearAPIKey {
		for _, k := range []string{"api_key", "api", "key_env", "api_key_env"} {
			delete(modelCfg, k)
		}
	}
	if clearAPIMode {
		delete(modelCfg, "api_mode")
	}
	if clearBaseURL {
		delete(modelCfg, "base_url")
	}
	return modelCfg
}

// ConfigWriteRefusedError is a config.yaml that cannot be read and so must not be replaced
type ConfigWriteRefusedError struct {
	Path   string
	Reason string
	Err    error
}

func (e *ConfigWriteRefusedError) Error() string {
	return fmt.Sprintf("Refusing to write %s: the file %s (%v). Fix it, then retry.", e.Path, e.Reason, e.Err)
}

func (e *ConfigWriteRefusedError) Unwrap() error { return e.Err }

// Refuses to replace an existing config.yaml that cannot be read or parsed, returning the raw mapping
func requireReadableConfigBeforeWrite(h string) (map[string]any, error) {
	if frozen := FrozenRuntimeConfig(); frozen != nil && workerConfigSnapshotActive() {
		return config.Clone(frozen).(map[string]any), nil
	}
	path := home.ConfigPath(h)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, &ConfigWriteRefusedError{Path: path, Reason: "cannot be accessed", Err: err}
	}
	loaded, err := config.ReadUser(h)
	if err != nil {
		var invalid *config.InvalidUserConfigError
		if errors.As(err, &invalid) {
			return nil, &ConfigWriteRefusedError{Path: path, Reason: "has a formatting error", Err: err}
		}
		return nil, &ConfigWriteRefusedError{Path: path, Reason: "cannot be read", Err: err}
	}
	if loaded == nil {
		return map[string]any{}, nil
	}
	return loaded, nil
}

// Persists the complete desired config state, omitted mapping keys being deliberately deleted
func atomicConfigReplace(h string, data map[string]any) error {
	return config.SaveUser(h, data)
}

func envHome(env PoolEnv) string {
	if env.Store != nil {
		return env.Store.Home()
	}
	return home.Process()
}

// Writes the active provider into auth.json and model.provider plus base_url into config.yaml
func updateConfigForProvider(env PoolEnv, providerID, inferenceBaseURL, defaultModel string, clearDefault bool) (string, error) {
	err := env.Store.Update(func(data AuthData) (bool, error) {
		data["active_provider"] = providerID
		return true, nil
	})
	if err != nil {
		return "", err
	}
	h := envHome(env)
	configPath := home.ConfigPath(h)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return "", err
	}
	if _, err := requireReadableConfigBeforeWrite(h); err != nil {
		return "", err
	}
	cfg, err := config.ReadUser(h)
	if err != nil {
		return "", err
	}
	modelCfg := map[string]any{}
	switch current := cfg["model"].(type) {
	case map[string]any:
		for k, v := range current {
			modelCfg[k] = v
		}
	case string:
		if strings.TrimSpace(current) != "" {
			modelCfg["default"] = strings.TrimSpace(current)
		}
	}
	modelCfg["provider"] = providerID
	if strings.TrimSpace(inferenceBaseURL) != "" {
		modelCfg["base_url"] = strings.TrimRight(inferenceBaseURL, "/")
	} else {
		delete(modelCfg, "base_url")
	}
	clearModelEndpointCredentials(modelCfg, true, true, false)
	if defaultModel != "" {
		curDefault := cfgString(modelCfg["default"])
		if curDefault == "" || strings.Contains(curDefault, "/") {
			modelCfg["default"] = defaultModel
		}
	} else if clearDefault {
		delete(modelCfg, "default")
	}
	cfg["model"] = modelCfg
	if err := atomicConfigReplace(h, cfg); err != nil {
		return "", err
	}
	return configPath, nil
}

// Normalized model.provider from config.yaml, empty when absent or unreadable
func getConfigProvider(env PoolEnv) string {
	cfg, err := config.ReadUser(envHome(env))
	if err != nil {
		return ""
	}
	model, _ := cfg["model"].(map[string]any)
	provider, _ := model["provider"].(string)
	return strings.ToLower(strings.TrimSpace(provider))
}

// True when logout should reset model.provider because config.yaml selects this registry provider
func shouldResetConfigProviderOnLogout(env PoolEnv, providerID string) bool {
	normalized := strings.ToLower(strings.TrimSpace(providerID))
	if _, ok := registryProfileCtx(env.ctx(), normalized); !ok {
		return false
	}
	return getConfigProvider(env) == normalized
}

// Logout target when auth.json names no active provider but config.yaml still selects an OAuth provider
func logoutDefaultProviderFromConfig(env PoolEnv) string {
	provider := getConfigProvider(env)
	flow, ok := OAuthProviderFlows[provider]
	if ok && flow.LogoutFromConfig {
		return provider
	}
	return ""
}

// Resets config.yaml model.provider to auto after logout
func resetConfigProvider(env PoolEnv) (string, error) {
	h := envHome(env)
	configPath := home.ConfigPath(h)
	if _, err := os.Stat(configPath); err != nil {
		return configPath, nil
	}
	if _, err := requireReadableConfigBeforeWrite(h); err != nil {
		return "", err
	}
	cfg, err := config.ReadUser(h)
	if err != nil {
		return "", err
	}
	if len(cfg) == 0 {
		return configPath, nil
	}
	if model, ok := cfg["model"].(map[string]any); ok {
		model["provider"] = "auto"
		if _, has := model["base_url"]; has {
			model["base_url"] = OpenRouterBaseURL
		}
	}
	if err := atomicConfigReplace(h, cfg); err != nil {
		return "", err
	}
	return configPath, nil
}

// LoginCommand prints the removal notice of the retired login command
func LoginCommand(out io.Writer) {
	fmt.Fprintln(out, "The 'icebath login' command has been removed.\nUse 'icebath auth' to manage credentials,\n'icebath model' to select a provider, or 'icebath setup' for full setup.")
}

// GetMinimaxOAuthAuthStatus is the MiniMax OAuth status snapshot
func GetMinimaxOAuthAuthStatus(env PoolEnv) map[string]any {
	state, err := env.Store.GetProviderAuthState("minimax-oauth")
	if err != nil || len(state) == 0 || anyString(state["access_token"]) == "" {
		return map[string]any{"logged_in": false, "provider": "minimax-oauth"}
	}
	tokenValid := true
	if expires, ok := ParseISOTimestamp(state["expires_at"]); ok {
		tokenValid = expires > nowEpoch()
	}
	region := anyString(state["region"])
	if region == "" {
		region = "global"
	}
	return map[string]any{"logged_in": tokenValid, "provider": "minimax-oauth", "region": region, "expires_at": state["expires_at"]}
}

// True when the registry knows the provider id
func isKnownAuthProvider(providerID string) bool {
	_, ok := registryProfile(strings.ToLower(strings.TrimSpace(providerID)))
	return ok
}

// Registry display name of a provider, else the id as given
func authProviderDisplayName(providerID string) string {
	if p, ok := registryProfile(strings.ToLower(strings.TrimSpace(providerID))); ok {
		return p.Label()
	}
	return providerID
}

// UnknownAuthProviderError is a logout aimed at a provider the registry does not know
type UnknownAuthProviderError struct{ Provider string }

func (e *UnknownAuthProviderError) Error() string { return "Unknown provider: " + e.Provider }

// LogoutCommand clears the auth state of the named, active, or config selected provider
func LogoutCommand(env PoolEnv, providerID string, out io.Writer) error {
	if providerID != "" && !isKnownAuthProvider(providerID) {
		return &UnknownAuthProviderError{Provider: providerID}
	}
	target := providerID
	if target == "" {
		target, _ = env.Store.ActiveProvider()
	}
	if target == "" {
		target = logoutDefaultProviderFromConfig(env)
	}
	if target == "" {
		fmt.Fprintln(out, "No provider is currently logged in.")
		return nil
	}
	if target == "nous" {
		state, _ := env.Store.GetProviderAuthState("nous")
		if isGuestState(state) {
			fmt.Fprintln(out, freeTierNotSignedIn())
			return nil
		}
	}
	shouldResetConfig := shouldResetConfigProviderOnLogout(env, target)
	providerName := authProviderDisplayName(target)
	cleared, err := env.Store.ClearProviderAuth(target)
	if err != nil {
		return err
	}
	if !cleared && !shouldResetConfig {
		fmt.Fprintf(out, "No auth state found for %s.\n", providerName)
		return nil
	}
	if target == "nous" {
		clearSharedNousState(env, "logout")
	}
	if shouldResetConfig {
		if _, err := resetConfigProvider(env); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Logged out of %s.\n", providerName)
	switch {
	case !shouldResetConfig:
		fmt.Fprintln(out, "Model provider configuration was unchanged.")
	case GetEnvPreferDotenv(env, "OPENROUTER_API_KEY") != "":
		fmt.Fprintln(out, "Icebath will use OpenRouter for inference.")
	default:
		fmt.Fprintln(out, "Run `icebath model` or configure an API key to use Icebath.")
	}
	return nil
}

// ---- format_auth_error ----

// Entitlement failures get fixed generic copy, Nous a Portal aware message
var genericEntitlementMessages = map[string]string{
	"subscription_required": "No active paid subscription found. Please purchase/activate a subscription, then retry.",
	"insufficient_credits":  "Subscription credits are exhausted. Top up/renew credits, then retry.",
}

var entitlementErrorCodes = map[string]bool{
	"subscription_required": true, "insufficient_credits": true, "subscription_expired": true,
	"no_usable_credits": true, "account_missing": true, "member_spend_cap_exceeded": true,
}

// Maps an auth failure onto concise user facing guidance
func formatAuthError(err *AuthError) string {
	if err == nil {
		return ""
	}
	if isRateLimitedAuthError(err) {
		return err.Error()
	}
	if err.ReloginRequired {
		return fmt.Sprintf("%s Run `icebath %smodel` to re-authenticate.", err.Error(), ProfileCLISelector(home.Process()))
	}
	if entitlementErrorCodes[err.Code] {
		if err.Provider == "nous" {
			return formatNousEntitlementAuthError(err)
		}
		if generic := genericEntitlementMessages[err.Code]; generic != "" {
			return generic
		}
	}
	if err.Code == "temporarily_unavailable" {
		return err.Error() + " Please retry in a few seconds."
	}
	return err.Error()
}

// FormatAuthError maps an auth failure onto concise user facing guidance
func FormatAuthError(err *AuthError) string { return formatAuthError(err) }
