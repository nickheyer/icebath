package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const qwenRerun = "Re-run 'qwen auth qwen-oauth'."

// Client for the Qwen token refresh, swappable so tests aim it at a local server
var qwenHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Token endpoint the Qwen refresh posts to, swappable so tests aim it at a local server
var qwenOAuthTokenURL = QwenOAuthTokenURL

// Token refresh of the Qwen CLI credentials, the auth module attribute the hermes tests patch
var refreshQwenCLITokensFn = refreshQwenCLITokens

// Path of the Qwen CLI OAuth credentials file in the user home
func qwenCLIAuthPath() string {
	userHome, err := os.UserHomeDir()
	if err != nil {
		userHome = ""
	}
	return filepath.Join(userHome, ".qwen", "oauth_creds.json")
}

// Qwen CLI OAuth credentials, failing when the file is missing, unreadable or not an object
func readQwenCLITokens() (map[string]any, error) {
	authPath := qwenCLIAuthPath()
	raw, err := os.ReadFile(authPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, qwenErr("Qwen CLI credentials not found. Run 'qwen auth qwen-oauth' first.", "qwen_auth_missing", false)
	}
	var data any
	if err == nil {
		err = json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data)
	}
	if err != nil {
		ae := qwenErr(fmt.Sprintf("Failed to read Qwen CLI credentials from %s: %s", authPath, err), "qwen_auth_read_failed", false)
		ae.Err = err
		return nil, ae
	}
	tokens, ok := data.(map[string]any)
	if !ok {
		return nil, qwenErr(fmt.Sprintf("Invalid Qwen CLI credentials in %s.", authPath), "qwen_auth_invalid", false)
	}
	return tokens, nil
}

// Writes the Qwen CLI credentials file privately and returns its path
func saveQwenCLITokens(tokens map[string]any) (string, error) {
	authPath := qwenCLIAuthPath()
	return authPath, savePrivateJSON(authPath, tokens)
}

// True when the millisecond expiry is unparseable or falls within the skew of now
func qwenAccessTokenIsExpiring(expiryDateMS any, skew time.Duration) bool {
	expiryMS, ok := pyInt(expiryDateMS)
	if !ok {
		return true
	}
	if skew < 0 {
		skew = 0
	}
	return float64(time.Now().Add(skew).UnixNano())/1e6 >= float64(expiryMS)
}

// Refreshes the Qwen CLI tokens against the Qwen token endpoint and saves them back to the CLI file
func refreshQwenCLITokens(ctx context.Context, tokens map[string]any, timeout time.Duration) (map[string]any, error) {
	refreshToken := strings.TrimSpace(pyOrStr(tokens["refresh_token"]))
	if refreshToken == "" {
		return nil, qwenErr("Qwen OAuth refresh token missing. "+qwenRerun, "qwen_refresh_token_missing", false)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {QwenOAuthClientID}}
	resp, err := oauthPostForm(ctx, qwenHTTPClientFn(timeout), qwenOAuthTokenURL, form, formJSONHeaders)
	if err != nil {
		ae := qwenErr(fmt.Sprintf("Qwen OAuth refresh failed: %s", err), "qwen_refresh_failed", false)
		ae.Err = err
		return nil, ae
	}
	if resp.Status >= 400 {
		body := strings.TrimSpace(string(resp.Body))
		suffix := ""
		if body != "" {
			suffix = " Response: " + body
		}
		return nil, qwenErr("Qwen OAuth refresh failed. "+qwenRerun+suffix, "qwen_refresh_failed", false)
	}
	raw, err := resp.JSON()
	if err != nil {
		ae := qwenErr(fmt.Sprintf("Qwen OAuth refresh returned invalid JSON: %s", err), "qwen_refresh_invalid_json", false)
		ae.Err = err
		return nil, ae
	}
	payload, ok := raw.(map[string]any)
	if !ok || strings.TrimSpace(pyOrStr(payload["access_token"])) == "" {
		return nil, qwenErr("Qwen OAuth refresh response missing access_token.", "qwen_refresh_invalid_response", false)
	}
	expiresIn, ok := pyInt(payload["expires_in"])
	if !ok {
		expiresIn = 6 * 60 * 60
	}
	orDefault := func(key string, fallback any, last string) string {
		v, has := payload[key]
		if !has {
			v = fallback
		}
		if s := pyOrStr(v); s != "" {
			return strings.TrimSpace(s)
		}
		return last
	}
	tokenType := orDefault("token_type", tokensOr(tokens, "token_type", "Bearer"), "Bearer")
	if tokenType == "" {
		tokenType = "Bearer"
	}
	refreshed := map[string]any{
		"access_token":  strings.TrimSpace(pyOrStr(payload["access_token"])),
		"refresh_token": strings.TrimSpace(orDefault("refresh_token", refreshToken, refreshToken)),
		"token_type":    tokenType,
		"resource_url":  orDefault("resource_url", tokensOr(tokens, "resource_url", "portal.qwen.ai"), "portal.qwen.ai"),
		"expiry_date":   time.Now().UnixMilli() + int64(max(1, expiresIn))*1000,
	}
	if _, err := saveQwenCLITokens(refreshed); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// Value of key in tokens, or the fallback when the key is absent
func tokensOr(tokens map[string]any, key string, fallback any) any {
	if v, has := tokens[key]; has {
		return v
	}
	return fallback
}

// Makes qwen-oauth the active provider with a minimal state, the tokens staying in the Qwen CLI file
func markQwenOAuthActive(env PoolEnv, creds map[string]any) error {
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := env.Store.Load()
	if err != nil {
		return err
	}
	state := map[string]any{}
	if pyTruthy(creds["base_url"]) {
		state["base_url"] = pyStr(creds["base_url"])
	}
	if err := env.Store.StoreProviderState(data, "qwen-oauth", state, true); err != nil {
		return err
	}
	return env.Store.Save(data)
}

// Qwen runtime credentials from the Qwen CLI file, refreshed when forced or within the skew of expiry
func resolveQwenRuntimeCredentialsWith(env PoolEnv, forceRefresh, refreshIfExpiring bool, skew time.Duration) (map[string]any, error) {
	tokens, err := readQwenCLITokens()
	if err != nil {
		return nil, err
	}
	shouldRefresh := forceRefresh
	if !shouldRefresh && refreshIfExpiring {
		shouldRefresh = qwenAccessTokenIsExpiring(tokens["expiry_date"], skew)
	}
	if shouldRefresh {
		if tokens, err = refreshQwenCLITokensFn(env.ctx(), tokens, 20*time.Second); err != nil {
			return nil, err
		}
	}
	accessToken := strings.TrimSpace(pyOrStr(tokens["access_token"]))
	if accessToken == "" {
		return nil, qwenErr("Qwen OAuth access token missing. "+qwenRerun, "qwen_access_token_missing", false)
	}
	return map[string]any{
		"provider": "qwen-oauth", "base_url": qwenBaseURLFromEnv(), "api_key": accessToken, "source": "qwen-cli",
		"expires_at_ms": tokens["expiry_date"], "auth_file": qwenCLIAuthPath(),
	}, nil
}

// Qwen runtime credentials with the default refresh skew
func resolveQwenRuntimeCredentials(env PoolEnv, refreshIfExpiring bool) (map[string]any, error) {
	return resolveQwenRuntimeCredentialsWith(env, false, refreshIfExpiring, QwenAccessTokenRefreshSkew)
}

// Qwen OAuth status, refresh validated so stale CLI tokens never read as logged in
func getQwenAuthStatus(env PoolEnv) map[string]any {
	authPath := qwenCLIAuthPath()
	creds, err := resolveQwenRuntimeCredentials(env, true)
	if err != nil {
		return map[string]any{"logged_in": false, "auth_file": authPath, "error": err.Error()}
	}
	return map[string]any{
		"logged_in": true, "auth_file": authPath, "source": creds["source"], "api_key": creds["api_key"],
		"expires_at_ms": creds["expires_at_ms"],
	}
}
