package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	codexMissingAccessTokenMsg  = "Codex auth is missing access_token. Run `%s` to re-authenticate."
	codexMissingRefreshTokenMsg = "Codex auth is missing refresh_token. Run `%s` to re-authenticate."
	codexNoCredentialsMsg       = "No Codex credentials stored. Run `%s` to authenticate."
	codexDeviceIssuer           = "https://auth.openai.com"
	// Real OAuth and device auth payloads are a few hundred bytes
	codexAuthBodyMaxBytes = int64(1024 * 1024)
)

// Profile aware Codex sign in command for the env's home
func codexReloginCommand(env PoolEnv) string {
	return oauthReloginCommandIn(envHome(env), "openai-codex")
}

// Whole seconds of a Retry-After header
func parseRetryAfterSeconds(h http.Header) (int, bool) {
	d, ok := RetryAfterFromHeaders(h)
	if !ok {
		return 0, false
	}
	return int(d.Seconds()), true
}

func stripped(v any) string { return strings.TrimSpace(anyString(v)) }

// Resets a pool entry's cooldown and last error metadata to healthy
func clearPoolEntryStatus(entry map[string]any) {
	for _, field := range poolStatusFields {
		entry[field] = nil
	}
}

// Codex endpoint, the profile scoped override winning over the ChatGPT default
func codexBaseURL(env PoolEnv) string {
	if override := strings.TrimRight(strings.TrimSpace(GetEnvPreferDotenv(env, "ICEBATH_CODEX_BASE_URL")), "/"); override != "" {
		return override
	}
	return DefaultCodexBaseURL
}

// Profile scoped ICEBATH_CODEX_BASE_URL override, empty when unset or unreadable without a scope
func scopedCodexBaseURLOverride(env PoolEnv) (override string) {
	defer func() {
		if recover() != nil {
			override = ""
		}
	}()
	return strings.TrimRight(stripped(env.secretStr("ICEBATH_CODEX_BASE_URL")), "/")
}

// Base URL the chat route sends a pooled Codex credential to, the same rule the pool entry runtime applies
func codexPoolRouteBaseURL(env PoolEnv, entryBaseURL string) string {
	base := strings.TrimRight(stripped(entryBaseURL), "/")
	if override := scopedCodexBaseURLOverride(env); override != "" {
		return override
	}
	if base == "" || base == DefaultCodexBaseURL {
		modelCfg, _ := env.config()["model"].(map[string]any)
		if cfgURL := configBaseURLForProvider(modelCfg, "openai-codex"); cfgURL != "" {
			base = cfgURL
		}
	}
	if base == "" {
		return DefaultCodexBaseURL
	}
	return base
}

func codexRuntimeResult(env PoolEnv, apiKey, source string, lastRefresh any, baseURL string) map[string]any {
	if baseURL == "" {
		baseURL = codexBaseURL(env)
	}
	return map[string]any{
		"provider": "openai-codex", "base_url": baseURL, "api_key": apiKey,
		"source": source, "last_refresh": lastRefresh, "auth_mode": "chatgpt",
	}
}

// Loads the auth store, taking the cross process lock unless the caller already holds it
func loadAuthStoreMaybeLocked(env PoolEnv, lock bool) (AuthData, error) {
	if lock {
		unlock, err := env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	return env.Store.Load()
}

// codexTokenData is the stored Codex token pair and its last refresh stamp
type codexTokenData struct {
	tokens      map[string]any
	lastRefresh any
}

// Reads the Codex OAuth tokens from the Icebath auth store
func readCodexTokens(env PoolEnv, lock bool) (codexTokenData, error) {
	data, err := loadAuthStoreMaybeLocked(env, lock)
	if err != nil {
		return codexTokenData{}, err
	}
	state := env.Store.ProviderState(data, "openai-codex")
	if len(state) == 0 {
		return codexTokenData{}, codexErr(fmt.Sprintf(codexNoCredentialsMsg, codexReloginCommand(env)), "codex_auth_missing", true)
	}
	tokens, ok := state["tokens"].(map[string]any)
	if !ok {
		return codexTokenData{}, codexErr(fmt.Sprintf("Codex auth state is missing tokens. Run `%s` to re-authenticate.", codexReloginCommand(env)), "codex_auth_invalid_shape", true)
	}
	if stripped(tokens["access_token"]) == "" {
		return codexTokenData{}, codexErr(fmt.Sprintf(codexMissingAccessTokenMsg, codexReloginCommand(env)), "codex_auth_missing_access_token", true)
	}
	if stripped(tokens["refresh_token"]) == "" {
		return codexTokenData{}, codexErr(fmt.Sprintf(codexMissingRefreshTokenMsg, codexReloginCommand(env)), "codex_auth_missing_refresh_token", true)
	}
	return codexTokenData{tokens: tokens, lastRefresh: state["last_refresh"]}, nil
}

// Mirrors a fresh Codex re-auth into the device_code pool row and legacy singleton aliases only
func syncCodexPoolEntries(data AuthData, tokens map[string]any, lastRefresh string, previousSingletonTokens map[string]any) {
	accessToken := anyString(tokens["access_token"])
	if accessToken == "" {
		return
	}
	refreshToken := anyString(tokens["refresh_token"])
	entries := codexPoolEntriesIn(data)
	if entries == nil {
		return
	}
	prevAT := anyString(previousSingletonTokens["access_token"])
	for _, entry := range entries {
		source := anyString(entry["source"])
		isAlias := source == SourceManualDeviceCode && prevAT != "" && anyString(entry["access_token"]) == prevAT
		if !(source == "device_code" || isAlias) {
			continue
		}
		entry["access_token"] = accessToken
		if refreshToken != "" {
			entry["refresh_token"] = refreshToken
		}
		if lastRefresh != "" {
			entry["last_refresh"] = lastRefresh
		}
		clearPoolEntryStatus(entry)
	}
}

// The live Codex pool row maps of a store, nil when the store holds no list for the provider
func codexPoolEntriesIn(data AuthData) []map[string]any {
	pool, _ := data["credential_pool"].(map[string]any)
	if pool == nil {
		return nil
	}
	list, ok := pool["openai-codex"].([]any)
	if !ok {
		return nil
	}
	return poolEntries(list)
}

// Saves Codex OAuth tokens to the store the grant was resolved from, root for a borrowed refresh
func saveCodexTokens(env PoolEnv, tokens map[string]any, lastRefresh, label string, setActive, writeThrough bool) error {
	if lastRefresh == "" {
		lastRefresh = utcNowZ()
	}
	return env.Store.ProviderTransaction("openai-codex", AuthLockTimeout, func(tx *ProviderTxn) error {
		state := map[string]any{}
		for k, v := range tx.State {
			state[k] = v
		}
		previousSingletonTokens, _ := state["tokens"].(map[string]any)
		state["tokens"] = tokens
		state["last_refresh"] = lastRefresh
		state["auth_mode"] = "chatgpt"
		if strings.TrimSpace(label) != "" {
			state["label"] = strings.TrimSpace(label)
		}
		targetData, targetPath := tx.Data, env.Store.Path()
		if writeThrough && tx.SourcePath != "" && !samePath(tx.SourcePath, env.Store.Path()) {
			rootData, err := env.Store.LoadFile(tx.SourcePath)
			if err != nil {
				return err
			}
			targetData, targetPath, setActive = rootData, tx.SourcePath, false
		}
		if err := env.Store.StoreProviderState(targetData, "openai-codex", state, setActive); err != nil {
			return err
		}
		syncCodexPoolEntries(targetData, tokens, lastRefresh, previousSingletonTokens)
		return env.Store.SaveTo(targetData, targetPath)
	})
}

// Adopts a valid Codex CLI token pair into the credential the caller observed, nil when nothing applies
func recoverCodexTokensFromCLI(env PoolEnv, reason, observedAccessToken string) map[string]any {
	if !AdoptExternalLoginsEnabled(env) {
		return nil
	}
	imported := importCodexCLITokensFn(env)
	if imported == nil || stripped(imported["access_token"]) == "" || stripped(imported["refresh_token"]) == "" {
		return nil
	}
	observed := stripped(observedAccessToken)
	adopted := false
	err := env.Store.ProviderTransaction("openai-codex", AuthLockTimeout, func(tx *ProviderTxn) error {
		stored, _ := tx.State["tokens"].(map[string]any)
		if stripped(stored["access_token"]) != observed {
			env.log().Info(fmt.Sprintf("Codex CLI recovery skipped (%s): the credential was re-authenticated meanwhile.", reason))
			return nil
		}
		knownAcc, knownSub, known := codexPrincipalIdentity(observed)
		if known {
			impAcc, impSub, impOK := codexPrincipalIdentity(anyString(imported["access_token"]))
			if impOK && (impAcc != knownAcc || impSub != knownSub) {
				env.log().Warn(fmt.Sprintf("Codex CLI recovery refused (%s): the Codex CLI login belongs to a different ChatGPT "+
					"workspace than the Icebath credential. Run `%s` to re-authenticate it.", reason, codexReloginCommand(env)))
				return nil
			}
		}
		env.log().Info(fmt.Sprintf("Codex auth recovered from Codex CLI auth.json (%s).", reason))
		if err := saveCodexTokens(env, imported, "", "", true, false); err != nil {
			return err
		}
		adopted = true
		return nil
	})
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex CLI recovery failed (%s): %v", reason, err))
		return nil
	}
	if !adopted {
		return nil
	}
	out := map[string]any{}
	for k, v := range imported {
		out[k] = v
	}
	return out
}

// oauthRefreshParse names the historical wording for each way a token refresh body can be wrong
type oauthRefreshParse struct {
	provider           string
	invalidJSON        [2]string
	invalidResponse    *[2]string
	missingAccess      [2]string
	reloginRequired    bool
	invalidJSONRelogin *bool
	strictStr          bool
}

// Parses a 200 token refresh response into its payload and stripped access token
func oauthRefreshPayloadAccessToken(resp *oauthHTTPResponse, spec oauthRefreshParse) (map[string]any, string, error) {
	mk := func(message, code string, relogin bool) *AuthError {
		return newAuthErr(spec.provider, message, code, relogin)
	}
	raw, err := resp.JSON()
	if err != nil {
		relogin := spec.reloginRequired
		if spec.invalidJSONRelogin != nil {
			relogin = *spec.invalidJSONRelogin
		}
		ae := mk(strings.ReplaceAll(spec.invalidJSON[0], "{exc}", err.Error()), spec.invalidJSON[1], relogin)
		ae.Err = err
		return nil, "", ae
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		if spec.invalidResponse != nil {
			return nil, "", mk(spec.invalidResponse[0], spec.invalidResponse[1], spec.reloginRequired)
		}
		payload = map[string]any{}
	}
	access := ""
	if spec.strictStr {
		if s, ok := payload["access_token"].(string); ok {
			access = strings.TrimSpace(s)
		}
	} else {
		access = stripped(payload["access_token"])
	}
	if access == "" {
		return nil, "", mk(spec.missingAccess[0], spec.missingAccess[1], spec.reloginRequired)
	}
	return payload, access, nil
}

var sslTroubleMarkers = []string{"[SSL:", "_ssl.c", "UNEXPECTED_EOF", "tls:", "x509:", "handshake failure"}

// Actionable hint for device login transport errors that look like TLS middlebox interference
func codexSSLInteropHint(err error) string {
	trouble := false
	for e := err; e != nil && !trouble; e = errors.Unwrap(e) {
		var certErr *tls.CertificateVerificationError
		var recordErr tls.RecordHeaderError
		var alertErr tls.AlertError
		var x509Unknown x509.UnknownAuthorityError
		var x509Hostname x509.HostnameError
		if errors.As(e, &certErr) || errors.As(e, &recordErr) || errors.As(e, &alertErr) || errors.As(e, &x509Unknown) || errors.As(e, &x509Hostname) || errors.Is(e, io.ErrUnexpectedEOF) {
			trouble = true
			break
		}
		text := e.Error()
		for _, marker := range sslTroubleMarkers {
			if strings.Contains(text, marker) {
				trouble = true
				break
			}
		}
	}
	if !trouble {
		return ""
	}
	return " This looks like a TLS handshake failure rather than a Codex outage: some networks reject" +
		" the larger TLS 1.3 ClientHello that Go sends by default (post-quantum hybrid" +
		" groups). Workaround: run with GODEBUG=tlsmlkem=0 to restrict key exchange to classic curves" +
		" (x25519:secp256r1:secp384r1:x448), or test with TLS 1.2 — see the Codex note in" +
		" https://hermes-agent.nousresearch.com/docs/integrations/providers"
}

// True when the error is transport level and safe to retry, decode errors and bugs surfacing at once
func codexIsTransientTransportError(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if _, wrapper := e.(*url.Error); wrapper {
			continue
		}
		if _, isAuth := e.(*AuthError); isAuth {
			return false
		}
		if e == io.EOF || e == io.ErrUnexpectedEOF || e == context.DeadlineExceeded || e == os.ErrDeadlineExceeded {
			return true
		}
		switch e.(type) {
		case *net.OpError, *net.DNSError, *os.SyscallError, syscall.Errno, *tls.CertificateVerificationError, tls.RecordHeaderError, tls.AlertError:
			return true
		}
		if _, isNet := e.(net.Error); isNet {
			return true
		}
	}
	return false
}

// Error for an auth body that ran past the cap, refusing to parse it
func codexBodyCapError(host string) error {
	return codexErr(fmt.Sprintf("Codex auth response from %s exceeded %d KiB; refusing to parse it.", host, codexAuthBodyMaxBytes/1024), "codex_auth_response_too_large", false)
}

// Client builder for the Codex OAuth and probe endpoints, swappable so tests aim it at a local server
var codexHTTPClientFn = codexHTTPClient

// Token endpoint the refresh and exchange calls post to, swappable so tests aim it at a local server
var codexOAuthTokenURL = CodexOAuthTokenURL

// Pure refresh the locked refresh and the pre probe refresh go through, swappable so tests script the endpoint
var codexRefreshPureFn = refreshCodexOAuthPure

// Codex CLI token import the recovery paths go through, swappable so tests script the CLI file
var importCodexCLITokensFn = importCodexCLITokens

// Live quota probe the persisted entry path goes through, swappable so tests script the usage endpoint
var probeCodexQuotaRestoredFn = probeCodexQuotaRestored

// SetCodexQuotaProbe swaps the live quota probe and returns the function that puts the previous one back
func SetCodexQuotaProbe(fn func(env PoolEnv, token, baseURL string) (bool, bool)) func() {
	prev := probeCodexQuotaRestoredFn
	probeCodexQuotaRestoredFn = fn
	return func() { probeCodexQuotaRestoredFn = prev }
}

// Client for Codex OAuth and probe endpoints racing address families with the proxy env normalized
func codexHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Runs one Codex request with the 1 MiB response body cap
func codexDo(client *http.Client, req *http.Request) (*oauthHTTPResponse, error) {
	return oauthDo(client, req, codexAuthBodyMaxBytes, codexBodyCapError)
}

func codexPost(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) (*oauthHTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return codexDo(client, req)
}

// One 15 second POST of the device login flow retried twice on a transient transport blip
func codexLoginPost(ctx context.Context, endpoint string, body []byte, headers map[string]string, failure [2]string) (*oauthHTTPResponse, error) {
	const attempts = 3
	for attempt := 1; ; attempt++ {
		resp, err := codexPost(ctx, codexHTTPClientFn(15*time.Second), endpoint, body, headers)
		if err == nil {
			return resp, nil
		}
		if attempt == attempts || !codexIsTransientTransportError(err) {
			ae := codexErr(fmt.Sprintf("%s: %v%s", failure[0], err, codexSSLInteropHint(err)), failure[1], false)
			ae.Err = err
			return nil, ae
		}
		oauthSleep(time.Duration(attempt) * time.Second)
	}
}

func codexQuotaExhaustedError(retryAfter int, hasRetryAfter bool) *AuthError {
	message := "Codex provider quota exhausted (429). Credentials are still valid; retry after the usage limit resets."
	if hasRetryAfter {
		message = fmt.Sprintf("Codex provider quota exhausted (429); retry after %ds. Credentials are still valid.", retryAfter)
	}
	ae := codexErr(message, CodexRateLimitedCode, false)
	if hasRetryAfter {
		ae.RetryAfter = time.Duration(retryAfter) * time.Second
		ae.HasRetryAfter = true
	}
	return ae
}

// Decodes a non 200 Codex token refresh response into a shaped auth error
func codexRefreshFailureError(resp *oauthHTTPResponse) *AuthError {
	code := "codex_refresh_failed"
	message := fmt.Sprintf("Codex token refresh failed with status %d.", resp.Status)
	if raw, err := resp.JSON(); err == nil {
		if payload, ok := raw.(map[string]any); ok {
			switch errObj := payload["error"].(type) {
			case map[string]any:
				nestedCode := errObj["code"]
				if stripped(nestedCode) == "" {
					nestedCode = errObj["type"]
				}
				if s, ok := nestedCode.(string); ok && strings.TrimSpace(s) != "" {
					code = strings.TrimSpace(s)
				}
				if s, ok := errObj["message"].(string); ok && strings.TrimSpace(s) != "" {
					message = "Codex token refresh failed: " + strings.TrimSpace(s)
				}
			case string:
				if strings.TrimSpace(errObj) != "" {
					code = strings.TrimSpace(errObj)
					desc := payload["error_description"]
					if s, ok := desc.(string); !ok || strings.TrimSpace(s) == "" {
						desc = payload["message"]
					}
					if s, ok := desc.(string); ok && strings.TrimSpace(s) != "" {
						message = "Codex token refresh failed: " + strings.TrimSpace(s)
					}
				}
			}
		}
	}
	if code == "refresh_token_reused" {
		message = "Codex refresh token was already consumed by another client (e.g. Codex CLI or VS Code extension). " +
			"Run `codex` in your terminal to generate fresh tokens, then run `" + OAuthReloginCommand("openai-codex") + "` to re-authenticate."
	}
	relogin := code == "invalid_grant" || code == "invalid_token" || code == "invalid_request" || code == "refresh_token_reused" ||
		resp.Status == 401 || resp.Status == 403
	return codexErr(message, code, relogin)
}

// Refreshes Codex OAuth tokens without touching the auth store
func refreshCodexOAuthPure(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
	_ = accessToken
	if strings.TrimSpace(refreshToken) == "" {
		return nil, codexErr(fmt.Sprintf(codexMissingRefreshTokenMsg, OAuthReloginCommand("openai-codex")), "codex_auth_missing_refresh_token", true)
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {CodexOAuthClientID}}
	resp, err := codexPost(ctx, codexHTTPClientFn(timeout), codexOAuthTokenURL, []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", "User-Agent": UserAgent("icebath-cli")})
	if err != nil {
		return nil, err
	}
	if resp.Status == 429 {
		retryAfter, has := parseRetryAfterSeconds(resp.Header)
		return nil, codexQuotaExhaustedError(retryAfter, has)
	}
	if resp.Status != 200 {
		return nil, codexRefreshFailureError(resp)
	}
	noRelogin := false
	payload, refreshedAccess, err := oauthRefreshPayloadAccessToken(resp, oauthRefreshParse{
		provider:           "openai-codex",
		invalidJSON:        [2]string{"Codex token refresh returned invalid JSON.", "codex_refresh_invalid_json"},
		invalidJSONRelogin: &noRelogin,
		missingAccess:      [2]string{"Codex token refresh response was missing access_token.", "codex_refresh_missing_access_token"},
		reloginRequired:    true,
		strictStr:          true,
	})
	if err != nil {
		return nil, err
	}
	updated := map[string]any{"access_token": refreshedAccess, "refresh_token": strings.TrimSpace(refreshToken), "last_refresh": utcNowZ()}
	if next, ok := payload["refresh_token"].(string); ok && strings.TrimSpace(next) != "" {
		updated["refresh_token"] = strings.TrimSpace(next)
	}
	return updated, nil
}

// Refreshes the Codex access token inside the source store's transaction, adopting a peer's rotation
func refreshCodexAuthTokens(env PoolEnv, tokens map[string]any, timeout time.Duration) (map[string]any, error) {
	lockTimeout := timeout + 5*time.Second
	if lockTimeout < AuthLockTimeout {
		lockTimeout = AuthLockTimeout
	}
	var result map[string]any
	err := env.Store.ProviderTransaction("openai-codex", lockTimeout, func(tx *ProviderTxn) error {
		stored, _ := tx.State["tokens"].(map[string]any)
		storedAT, storedRT := stripped(stored["access_token"]), stripped(stored["refresh_token"])
		if storedAT != "" && storedRT != "" && storedRT != stripped(tokens["refresh_token"]) {
			env.log().Info("Codex refresh token already rotated by a peer — adopting the stored pair.")
			result = mergeTokens(tokens, map[string]any{"access_token": storedAT, "refresh_token": storedRT})
			return nil
		}
		refreshed, err := codexRefreshPureFn(env.ctx(), anyString(tokens["access_token"]), anyString(tokens["refresh_token"]), timeout)
		if err != nil {
			var authErr *AuthError
			if !errors.As(err, &authErr) || !authErr.ReloginRequired {
				return err
			}
			code := authErr.Code
			if code == "" {
				code = "auth_error"
			}
			imported := recoverCodexTokensFromCLI(env, "refresh_token rejected: "+code, storedAT)
			if imported == nil {
				return err
			}
			result = imported
			return nil
		}
		updatedTokens := mergeTokens(tokens, map[string]any{"access_token": refreshed["access_token"], "refresh_token": refreshed["refresh_token"]})
		if err := saveCodexTokens(env, updatedTokens, "", "", true, true); err != nil {
			return err
		}
		result = updatedTokens
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func mergeTokens(base, overrides map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// Reads the Codex CLI auth.json tokens when valid and unexpired, never writing
func importCodexCLITokens(env PoolEnv) map[string]any {
	authPath := filepath.Join(codexHomeDir(), "auth.json")
	info, err := os.Stat(authPath)
	if err != nil || info.IsDir() {
		return nil
	}
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return nil
	}
	tokens, ok := doc["tokens"].(map[string]any)
	if !ok || anyString(tokens["access_token"]) == "" || anyString(tokens["refresh_token"]) == "" {
		return nil
	}
	if codexAccessTokenIsExpiring(anyString(tokens["access_token"]), 0) {
		env.log().Debug(fmt.Sprintf("Codex CLI tokens at %s are expired — skipping import.", authPath))
		return nil
	}
	out := map[string]any{}
	for k, v := range tokens {
		out[k] = v
	}
	return out
}

// CodexResolveOptions tune the Codex runtime credential resolution
type CodexResolveOptions struct {
	ForceRefresh        bool
	SkipExpiringRefresh bool
	RefreshSkew         time.Duration
	ReadOnly            bool
}

// Read only Codex credentials for status, doctor, and pickers, never adopting, refreshing, or writing
func resolveCodexRuntimeCredentialsReadOnly(env PoolEnv) (map[string]any, error) {
	return resolveCodexRuntimeCredentials(env, CodexResolveOptions{ReadOnly: true})
}

// Resolves runtime credentials from the Icebath Codex token store, the pool serving when the singleton cannot
func resolveCodexRuntimeCredentials(env PoolEnv, opts CodexResolveOptions) (map[string]any, error) {
	skew := opts.RefreshSkew
	if skew <= 0 {
		skew = CodexAccessTokenRefreshSkew
	}
	var readError *AuthError
	var data *codexTokenData
	observed := ""
	readTokens := func() (codexTokenData, error) {
		if opts.ReadOnly {
			return readCodexTokens(env, false)
		}
		unlock, err := env.Store.Lock(AuthLockTimeout)
		if err != nil {
			return codexTokenData{}, err
		}
		defer unlock()
		store, err := env.Store.Load()
		if err != nil {
			return codexTokenData{}, err
		}
		if raw, ok := env.Store.ProviderState(store, "openai-codex")["tokens"].(map[string]any); ok {
			observed = anyString(raw["access_token"])
		}
		return readCodexTokens(env, false)
	}
	read, err := readTokens()
	if err != nil {
		var authErr *AuthError
		if !errors.As(err, &authErr) {
			return nil, err
		}
		readError = authErr
		if !opts.ReadOnly && authErr.ReloginRequired && (authErr.Code == "codex_auth_missing_access_token" || authErr.Code == "codex_auth_missing_refresh_token" || authErr.Code == "codex_auth_invalid_shape") {
			code := authErr.Code
			if code == "" {
				code = "auth_error"
			}
			if imported := recoverCodexTokensFromCLI(env, code, observed); imported != nil {
				data = &codexTokenData{tokens: imported, lastRefresh: imported["last_refresh"]}
			}
		}
	} else {
		data = &read
	}
	if data == nil {
		poolToken, poolBase := poolCodexCredential(env)
		if poolToken != "" && opts.ForceRefresh && !opts.ReadOnly {
			refreshed, ok := LoadPool(env, "openai-codex").TryRefreshMatching(poolToken, "")
			poolToken = ""
			if ok {
				poolToken = refreshed.RuntimeAPIKey()
			}
		}
		if poolToken != "" {
			return codexRuntimeResult(env, poolToken, "credential_pool", nil, codexPoolRouteBaseURL(env, poolBase)), nil
		}
		if poolRateLimit := codexPoolRateLimitStatus(env); poolRateLimit != nil {
			if !opts.ReadOnly {
				if restored, known := probeCodexPoolEntryQuotaRestored(env, poolRateLimit); known && restored {
					env.log().Info("Codex quota restored upstream — clearing stale pool cooldown(s).")
					ClearCodexPoolQuotaCooldowns(env, "")
					poolToken, poolBase = poolCodexCredential(env)
					if poolToken != "" {
						return codexRuntimeResult(env, poolToken, "credential_pool", nil, codexPoolRouteBaseURL(env, poolBase)), nil
					}
				}
			}
			resetAt, ok := anyFloat(poolRateLimit["reset_at"])
			if ok && resetAt > nowEpoch() {
				return nil, codexQuotaExhaustedError(int(resetAt-nowEpoch()), true)
			}
			return nil, codexQuotaExhaustedError(0, false)
		}
		if readError != nil {
			return nil, readError
		}
		return nil, codexErr(fmt.Sprintf(codexNoCredentialsMsg, codexReloginCommand(env)), "codex_auth_missing", true)
	}
	tokens := mergeTokens(data.tokens, nil)
	accessToken := stripped(tokens["access_token"])
	refreshTimeout := time.Duration(envFloat("ICEBATH_CODEX_REFRESH_TIMEOUT_SECONDS", 20) * float64(time.Second))
	shouldRefresh := func(token string) bool {
		if opts.ReadOnly {
			return false
		}
		return opts.ForceRefresh || (!opts.SkipExpiringRefresh && codexAccessTokenIsExpiring(token, skew))
	}
	if shouldRefresh(accessToken) {
		lockTimeout := refreshTimeout + 5*time.Second
		if lockTimeout < AuthLockTimeout {
			lockTimeout = AuthLockTimeout
		}
		unlock, err := env.Store.Lock(lockTimeout)
		if err != nil {
			return nil, err
		}
		defer unlock()
		fresh, err := readCodexTokens(env, false)
		if err != nil {
			return nil, err
		}
		data = &fresh
		tokens = mergeTokens(fresh.tokens, nil)
		if shouldRefresh(stripped(tokens["access_token"])) {
			tokens, err = refreshCodexAuthTokens(env, tokens, refreshTimeout)
			if err != nil {
				return nil, err
			}
		}
		accessToken = stripped(tokens["access_token"])
	}
	return codexRuntimeResult(env, accessToken, "icebath-auth-store", data.lastRefresh, ""), nil
}

// Exported Codex runtime credential resolution for the agent client refresh
func ResolveCodexRuntimeCredentials(env PoolEnv, opts CodexResolveOptions) (map[string]any, error) {
	return resolveCodexRuntimeCredentials(env, opts)
}

// Pool entry frozen by a 429 or quota stop rather than an auth failure
func entryIsRateLimitExhausted(entry map[string]any) bool {
	if anyString(entry["last_status"]) != StatusExhausted {
		return false
	}
	code, _ := anyFloat(entry["last_error_code"])
	return isCodexRateLimitShaped(int(code), anyString(entry["last_error_reason"]), anyString(entry["last_error_message"]))
}

type codexProbeResult struct {
	at       time.Time
	restored bool
	known    bool
}

// Per token throttle of the live quota probe so the hot selection path can fire it freely
var (
	codexQuotaProbeMu    sync.Mutex
	codexQuotaProbeCache = map[string]codexProbeResult{}
)

func codexQuotaProbeCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// Codex usage endpoint for a probe, ChatGPT backends on wham and everything else on api codex
func codexUsageProbeURL(env PoolEnv, baseURL string) string {
	normalized := strings.TrimRight(stripped(baseURL), "/")
	if normalized == "" {
		normalized = codexBaseURL(env)
	}
	normalized = strings.TrimSuffix(normalized, "/codex")
	if strings.Contains(normalized, "/backend-api") {
		return normalized + "/wham/usage"
	}
	return normalized + "/api/codex/usage"
}

// Asks the Codex usage endpoint whether the account's quota is usable again, throttled per token
func probeCodexQuotaRestoredWith(env PoolEnv, accessToken, baseURL string, minInterval time.Duration) (bool, bool) {
	token := stripped(accessToken)
	if token == "" || len(DecodeJWTClaims(token)) == 0 {
		return false, false
	}
	cacheKey := codexQuotaProbeCacheKey(token)
	now := time.Now()
	codexQuotaProbeMu.Lock()
	if cached, ok := codexQuotaProbeCache[cacheKey]; ok && now.Sub(cached.at) < minInterval {
		codexQuotaProbeMu.Unlock()
		return cached.restored, cached.known
	}
	codexQuotaProbeCache[cacheKey] = codexProbeResult{at: now}
	codexQuotaProbeMu.Unlock()
	restored, known := codexQuotaProbeRequest(env, token, baseURL)
	codexQuotaProbeMu.Lock()
	codexQuotaProbeCache[cacheKey] = codexProbeResult{at: now, restored: restored, known: known}
	codexQuotaProbeMu.Unlock()
	return restored, known
}

// One usage request, restored when every window sits below one hundred percent
func codexQuotaProbeRequest(env PoolEnv, token, baseURL string) (bool, bool) {
	req, err := http.NewRequestWithContext(env.ctx(), http.MethodGet, codexUsageProbeURL(env, baseURL), nil)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex quota probe failed: %v", err))
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli")
	for k, v := range codexAccountHeaders(token) {
		req.Header.Set(k, v)
	}
	resp, err := codexDo(codexHTTPClientFn(10*time.Second), req)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex quota probe failed: %v", err))
		return false, false
	}
	if resp.Status == 429 {
		return false, true
	}
	if resp.Status != 200 {
		return false, false
	}
	payload, err := resp.JSONObject()
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex quota probe failed: %v", err))
		return false, false
	}
	windows := []map[string]any{}
	if rl, ok := payload["rate_limit"].(map[string]any); ok {
		windows = append(windows, rl)
	}
	if extras, ok := payload["additional_rate_limits"].([]any); ok {
		for _, extra := range extras {
			if m, ok := extra.(map[string]any); ok {
				if rl, ok := m["rate_limit"].(map[string]any); ok {
					windows = append(windows, rl)
				}
			}
		}
	}
	worstUsed, have := 0.0, false
	for _, rateLimit := range windows {
		for _, key := range []string{"primary_window", "secondary_window"} {
			window, _ := rateLimit[key].(map[string]any)
			if used, ok := anyFloat(window["used_percent"]); ok {
				if !have || used > worstUsed {
					worstUsed = used
				}
				have = true
			}
		}
	}
	if !have {
		return false, false
	}
	return worstUsed < 100, true
}

// Live Codex quota probe with the default throttle
func probeCodexQuotaRestored(env PoolEnv, token, baseURL string) (bool, bool) {
	return probeCodexQuotaRestoredWith(env, token, baseURL, CodexQuotaProbeMinInterval)
}

// Refreshes an expired stored access token so the quota probe gets a real answer, the throttle shared
func refreshExpiredCodexProbeTokenWith(env PoolEnv, accessToken, refreshToken string, minInterval time.Duration) (map[string]any, bool) {
	token, refresh := stripped(accessToken), stripped(refreshToken)
	if token == "" || refresh == "" || !codexAccessTokenIsExpiring(token, 0) {
		return nil, false
	}
	cacheKey := codexQuotaProbeCacheKey(token)
	now := time.Now()
	codexQuotaProbeMu.Lock()
	cached, ok := codexQuotaProbeCache[cacheKey]
	codexQuotaProbeMu.Unlock()
	if ok && now.Sub(cached.at) < minInterval {
		return nil, false
	}
	fresh, err := codexRefreshPureFn(env.ctx(), token, refresh, 20*time.Second)
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex pre-probe token refresh failed: %v", err))
		codexQuotaProbeMu.Lock()
		codexQuotaProbeCache[cacheKey] = codexProbeResult{at: now}
		codexQuotaProbeMu.Unlock()
		return nil, false
	}
	return fresh, true
}

// Pre probe refresh with the default throttle
func refreshExpiredCodexProbeToken(env PoolEnv, token, refreshToken string) (map[string]any, bool) {
	return refreshExpiredCodexProbeTokenWith(env, token, refreshToken, CodexQuotaProbeMinInterval)
}

// Quota probe for a persisted pool entry, an expired token refreshed and persisted first
func probeCodexPoolEntryQuotaRestored(env PoolEnv, entry map[string]any) (bool, bool) {
	token := stripped(entry["access_token"])
	if fresh, ok := refreshExpiredCodexProbeToken(env, token, anyString(entry["refresh_token"])); ok {
		token = anyString(fresh["access_token"])
		err := env.Store.Update(func(data AuthData) (bool, error) {
			for _, diskEntry := range codexPoolEntriesIn(data) {
				if anyString(diskEntry["id"]) == anyString(entry["id"]) {
					for k, v := range fresh {
						diskEntry[k] = v
					}
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			env.log().Debug(fmt.Sprintf("Failed to persist refreshed Codex pool tokens: %v", err))
		}
	}
	if token == "" {
		return false, false
	}
	return probeCodexQuotaRestoredFn(env, token, codexPoolRouteBaseURL(env, anyString(entry["base_url"])))
}

// ClearCodexPoolQuotaCooldowns lifts rate limit cooldowns on persisted Codex pool rows, one token or every row
func ClearCodexPoolQuotaCooldowns(env PoolEnv, accessToken string) int {
	cleared := 0
	target := ""
	if !profileOwnsPoolProvider(env, "openai-codex") {
		target = borrowedSingleUsePoolRoot(env)
	}
	path := target
	if path == "" {
		path = env.Store.Path()
	}
	err := func() error {
		unlock, err := env.Store.fileLock(path, AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := env.Store.LoadFile(path)
		if err != nil {
			return err
		}
		for _, entry := range codexPoolEntriesIn(data) {
			if accessToken != "" && anyString(entry["access_token"]) != accessToken {
				continue
			}
			if entryIsRateLimitExhausted(entry) {
				clearPoolEntryStatus(entry)
				cleared++
			}
		}
		if cleared > 0 {
			return env.Store.SaveTo(data, path)
		}
		return nil
	}()
	if err != nil {
		env.log().Debug(fmt.Sprintf("Failed to clear Codex pool quota cooldowns: %v", err))
		return 0
	}
	return cleared
}

// Metadata of a pool only Codex credential in quota cooldown, read through the global root fallback
func codexPoolRateLimitStatus(env PoolEnv) map[string]any {
	entries, err := env.Store.ReadCredentialPool("openai-codex")
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex pool rate-limit lookup failed: %v", err))
		return nil
	}
	now := nowEpoch()
	for _, entry := range entries {
		token, _ := entry["access_token"].(string)
		if strings.TrimSpace(token) == "" || !entryIsRateLimitExhausted(entry) {
			continue
		}
		resetAt, ok := ParseAbsoluteTimestamp(entry["last_error_reset_at"])
		if !ok || resetAt > now {
			var reset any
			if ok {
				reset = resetAt
			}
			return map[string]any{
				"label": entry["label"], "last_refresh": entry["last_refresh"], "reset_at": reset,
				"reason": entry["last_error_reason"], "message": entry["last_error_message"],
				"access_token": strings.TrimSpace(token), "refresh_token": entry["refresh_token"],
				"id": entry["id"], "base_url": entry["base_url"],
			}
		}
	}
	return nil
}

// Access token and row base URL of the first usable pool row outside a cooldown window
func poolCodexCredential(env PoolEnv) (string, string) {
	entries, err := env.Store.ReadCredentialPool("openai-codex")
	if err != nil {
		env.log().Debug(fmt.Sprintf("Codex pool fallback lookup failed: %v", err))
		return "", ""
	}
	for _, entry := range entries {
		token, _ := entry["access_token"].(string)
		resetAt, ok := ParseAbsoluteTimestamp(entry["last_error_reset_at"])
		inCooldown := ok && resetAt > nowEpoch()
		if strings.TrimSpace(token) != "" && !inCooldown {
			return strings.TrimSpace(token), stripped(entry["base_url"])
		}
	}
	return "", ""
}

// CodexLoginArgs are the flags of an interactive Codex login
type CodexLoginArgs struct {
	Browser   bool
	NoBrowser bool
	Timeout   time.Duration
}

// LoginOpenAICodex runs the Codex login, device code by default and browser PKCE when opted in
func LoginOpenAICodex(env PoolEnv, out io.Writer, args CodexLoginArgs, forceNewLogin bool) error {
	if !forceNewLogin {
		accepted, err := offerExistingOAuthCredentials(env, out, "openai-codex",
			func() (map[string]any, error) { return resolveCodexRuntimeCredentials(env, CodexResolveOptions{}) },
			codexAccessTokenIsExpiring, "Codex", DefaultCodexBaseURL, "Existing Codex credentials are expired. Starting fresh login...")
		if err != nil {
			return err
		}
		if accepted {
			return nil
		}
		if cliTokens := importCodexCLITokensFn(env); cliTokens != nil {
			fmt.Fprintln(out, "Found existing Codex CLI credentials at ~/.codex/auth.json")
			fmt.Fprintln(out, "Icebath will create its own session to avoid conflicts with Codex CLI / VS Code.")
			if promptYesNo("Import these credentials? (a separate login is recommended) [y/N]: ", "n") {
				if err := saveCodexTokens(env, cliTokens, "", "", true, false); err != nil {
					return err
				}
				configPath, err := updateConfigForProvider(env, "openai-codex", codexBaseURL(env), "", false)
				if err != nil {
					return err
				}
				fmt.Fprintln(out)
				fmt.Fprintln(out, "Credentials imported. Note: if Codex CLI refreshes its token,")
				fmt.Fprintln(out, "Icebath will keep working independently with its own session.")
				fmt.Fprintf(out, "  Config updated: %s (model.provider=openai-codex)\n", configPath)
				return nil
			}
		}
	}
	fmt.Fprintln(out)
	creds, err := codexOAuthLogin(env, out, args)
	if err != nil {
		return err
	}
	tokens, _ := creds["tokens"].(map[string]any)
	if err := saveCodexTokens(env, tokens, anyString(creds["last_refresh"]), "", true, false); err != nil {
		return err
	}
	baseURL := anyString(creds["base_url"])
	if _, has := creds["base_url"]; !has {
		baseURL = DefaultCodexBaseURL
	}
	configPath, err := updateConfigForProvider(env, "openai-codex", baseURL, "", false)
	if err != nil {
		return err
	}
	printLoginSuccess(out, envHome(env), "openai-codex", configPath, true)
	return nil
}

// Error for a 429 from the device auth endpoints, a throttle rather than a credential fault
func codexLoginRateLimitedError(resp *oauthHTTPResponse, during string) *AuthError {
	waitHint := " Wait a minute and run the login again."
	if retryAfter, ok := parseRetryAfterSeconds(resp.Header); ok {
		waitHint = fmt.Sprintf(" Try again in about %ds.", retryAfter)
	}
	return codexErr(fmt.Sprintf("OpenAI is rate-limiting Codex login requests (HTTP 429)%s. This is a temporary throttle on OpenAI's side, not a credential problem.%s", during, waitHint), CodexRateLimitedCode, false)
}

// Step one of the device flow, requesting a user code with capped retries on HTTP 429
func codexRequestDeviceCode(ctx context.Context, out io.Writer, issuer, clientID string) (map[string]any, error) {
	const maxAttempts = 4
	var resp *oauthHTTPResponse
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var err error
		resp, err = codexLoginPost(ctx, issuer+"/api/accounts/deviceauth/usercode", body, map[string]string{"Content-Type": "application/json"},
			[2]string{"Failed to request device code", "device_code_request_failed"})
		if err != nil {
			return nil, err
		}
		if resp.Status != 429 {
			break
		}
		if attempt < maxAttempts {
			delay := 1 << attempt
			if retryAfter, ok := parseRetryAfterSeconds(resp.Header); ok {
				delay = retryAfter
			}
			if delay < 1 {
				delay = 1
			}
			if delay > 60 {
				delay = 60
			}
			fmt.Fprintf(out, "OpenAI is rate-limiting login requests (429); retrying in %ds...\n", delay)
			oauthSleep(time.Duration(delay) * time.Second)
		}
	}
	if resp.Status == 429 {
		return nil, codexLoginRateLimitedError(resp, "")
	}
	if resp.Status != 200 {
		return nil, codexErr(fmt.Sprintf("Device code request returned status %d.", resp.Status), "device_code_request_error", false)
	}
	deviceData, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	interval := 5
	switch v := deviceData["interval"].(type) {
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, err
		}
		interval = parsed
	case float64:
		interval = int(v)
	case int:
		interval = v
	}
	if interval < 3 {
		interval = 3
	}
	deviceData["interval"] = interval
	if anyString(deviceData["user_code"]) == "" || anyString(deviceData["device_auth_id"]) == "" {
		return nil, codexErr("Device code response missing required fields.", "device_code_incomplete", false)
	}
	return deviceData, nil
}

// Step three of the device flow, polling until sign in completes with 403 and 404 meaning still pending
func codexPollAuthorizationCode(ctx context.Context, out io.Writer, issuer, deviceAuthID, userCode string, pollInterval int) (map[string]any, error) {
	const maxWait = 15 * time.Minute
	const maxConsecutiveBlips = 6
	start := time.Now()
	client := codexHTTPClientFn(15 * time.Second)
	body, _ := json.Marshal(map[string]string{"device_auth_id": deviceAuthID, "user_code": userCode})
	consecutiveBlips := 0
	for time.Since(start) < maxWait {
		oauthSleep(time.Duration(pollInterval) * time.Second)
		if ctx.Err() != nil {
			fmt.Fprintln(out, "\nLogin cancelled.")
			return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		pollResp, err := codexPost(ctx, client, issuer+"/api/accounts/deviceauth/token", body, map[string]string{"Content-Type": "application/json"})
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(out, "\nLogin cancelled.")
				return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
			}
			if !codexIsTransientTransportError(err) {
				ae := codexErr(fmt.Sprintf("Device auth polling request failed: %v%s", err, codexSSLInteropHint(err)), "device_code_poll_error", false)
				ae.Err = err
				return nil, ae
			}
			consecutiveBlips++
			if consecutiveBlips >= maxConsecutiveBlips {
				ae := codexErr(fmt.Sprintf("Device auth polling request failed after %d consecutive transport errors: %v%s", consecutiveBlips, err, codexSSLInteropHint(err)), "device_code_poll_error", false)
				ae.Err = err
				return nil, ae
			}
			fmt.Fprintln(out, "Transient network error while waiting for sign-in; retrying...")
			continue
		}
		consecutiveBlips = 0
		if pollResp.Status == 200 {
			return pollResp.JSONObject()
		}
		if pollResp.Status != 403 && pollResp.Status != 404 {
			return nil, codexErr(fmt.Sprintf("Device auth polling returned status %d.", pollResp.Status), "device_code_poll_error", false)
		}
	}
	return nil, codexErr("Login timed out after 15 minutes.", "device_code_timeout", false)
}

// Step four of the device flow, swapping the authorization code for tokens
func codexExchangeAuthorizationCode(ctx context.Context, issuer, clientID string, codeResp map[string]any) (map[string]any, error) {
	authorizationCode := anyString(codeResp["authorization_code"])
	codeVerifier := anyString(codeResp["code_verifier"])
	if authorizationCode == "" || codeVerifier == "" {
		return nil, codexErr("Device auth response missing authorization_code or code_verifier.", "device_code_incomplete_exchange", false)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {authorizationCode}, "redirect_uri": {issuer + "/deviceauth/callback"},
		"client_id": {clientID}, "code_verifier": {codeVerifier}}
	return codexTokenExchange(ctx, form)
}

// Token endpoint exchange shared by the device and browser flows
func codexTokenExchange(ctx context.Context, form url.Values) (map[string]any, error) {
	tokenResp, err := codexLoginPost(ctx, codexOAuthTokenURL, []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		[2]string{"Token exchange failed", "token_exchange_failed"})
	if err != nil {
		return nil, err
	}
	if tokenResp.Status == 429 {
		return nil, codexLoginRateLimitedError(tokenResp, " during token exchange")
	}
	if tokenResp.Status != 200 {
		return nil, codexErr(fmt.Sprintf("Token exchange returned status %d.", tokenResp.Status), "token_exchange_error", false)
	}
	tokens, err := tokenResp.JSONObject()
	if err != nil {
		return nil, err
	}
	if anyString(tokens["access_token"]) == "" {
		return nil, codexErr("Token exchange did not return an access_token.", "token_exchange_no_access_token", false)
	}
	return tokens, nil
}

// Runs the OpenAI device code login and returns the credential shape for the caller to persist
func codexDeviceCodeLogin(env PoolEnv, out io.Writer) (map[string]any, error) {
	ctx := env.ctx()
	deviceData, err := codexRequestDeviceCode(ctx, out, codexDeviceIssuer, CodexOAuthClientID)
	if err != nil {
		return nil, err
	}
	userCode := anyString(deviceData["user_code"])
	fmt.Fprint(out, "To continue, follow these steps:\n\n")
	fmt.Fprintln(out, "  1. Open this URL in your browser:")
	fmt.Fprintf(out, "     \033[94m%s/codex/device\033[0m\n\n", codexDeviceIssuer)
	fmt.Fprintln(out, "  2. Enter this code:")
	fmt.Fprintf(out, "     \033[94m%s\033[0m\n\n", userCode)
	fmt.Fprintln(out, "Waiting for sign-in... (press Ctrl+C to cancel)")
	interval, _ := deviceData["interval"].(int)
	codeResp, err := codexPollAuthorizationCode(ctx, out, codexDeviceIssuer, anyString(deviceData["device_auth_id"]), userCode, interval)
	if err != nil {
		return nil, err
	}
	tokens, err := codexExchangeAuthorizationCode(ctx, codexDeviceIssuer, CodexOAuthClientID, codeResp)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"tokens":   map[string]any{"access_token": anyString(tokens["access_token"]), "refresh_token": anyString(tokens["refresh_token"])},
		"base_url": codexBaseURL(env), "last_refresh": utcNowZ(), "auth_mode": "chatgpt", "source": "device-code",
	}, nil
}
