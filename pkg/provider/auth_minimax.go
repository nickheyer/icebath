package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const minimaxOAuthErrorBodyLimit = 16 * 1024

// Client for the MiniMax OAuth endpoints, swappable so tests aim it at a local server
var minimaxHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Sleep between MiniMax token polls, swappable so tests run the poll loop without waiting
var minimaxSleepFn = time.Sleep

// OAuthLoginExitError carries the process exit code a failed provider login asks for
type OAuthLoginExitError struct {
	Code int
	Err  error
}

func (e *OAuthLoginExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("login exited with status %d", e.Code)
}

func (e *OAuthLoginExitError) Unwrap() error { return e.Err }

// minimaxResponse is a MiniMax OAuth reply whose error body is read only up to the cap
type minimaxResponse struct {
	Status    int
	Reason    string
	Body      []byte
	errorText string
}

// Bounded error body of a MiniMax OAuth response with a truncation marker past the limit
func minimaxResponseErrorText(body io.Reader, limit int) string {
	if limit < 0 {
		limit = 0
	}
	raw, _ := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	text := raw
	if len(text) > limit {
		text = text[:limit]
	}
	out := sanitizeSurrogates(string(text))
	if len(raw) > limit {
		out += "...[truncated]"
	}
	return out
}

// Posts a MiniMax OAuth form reading the full body only on success
func minimaxPostForm(ctx context.Context, client *http.Client, endpoint string, data url.Values, headers map[string]string) (*minimaxResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := &minimaxResponse{Status: resp.StatusCode, Reason: http.StatusText(resp.StatusCode)}
	if resp.StatusCode == 200 {
		if out.Body, err = io.ReadAll(resp.Body); err != nil {
			return nil, err
		}
		return out, nil
	}
	out.errorText = minimaxResponseErrorText(resp.Body, minimaxOAuthErrorBodyLimit)
	return out, nil
}

// Code verifier, S256 challenge and state for the MiniMax user code flow
func minimaxPKCEPair() (string, string, string) {
	verifier := pkceCodeVerifier(64)
	if len(verifier) > 96 {
		verifier = verifier[:96]
	}
	sum := sha256.Sum256([]byte(verifier))
	stateRaw := make([]byte, 16)
	fillRandom(stateRaw)
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), base64.RawURLEncoding.EncodeToString(stateRaw)
}

// Requests a MiniMax user code, checking the required fields and the echoed state
func minimaxRequestUserCode(ctx context.Context, client *http.Client, portalBaseURL, clientID, codeChallenge, state string) (map[string]any, error) {
	headers := map[string]string{"x-request-id": uuid.NewString()}
	for k, v := range formJSONHeaders {
		headers[k] = v
	}
	resp, err := minimaxPostForm(ctx, client, portalBaseURL+"/oauth/code", url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "scope": {MinimaxOAuthScope},
		"code_challenge": {codeChallenge}, "code_challenge_method": {"S256"}, "state": {state},
	}, headers)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		body := resp.errorText
		if body == "" {
			body = resp.Reason
		}
		return nil, minimaxErr("MiniMax OAuth authorization failed: "+body, "authorization_failed", false)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, err
	}
	for _, field := range []string{"user_code", "verification_uri", "expired_in"} {
		if _, has := payload[field]; !has {
			return nil, minimaxErr("MiniMax OAuth response missing field: "+field, "authorization_incomplete", false)
		}
	}
	if got, ok := payload["state"].(string); !ok || got != state {
		return nil, minimaxErr("MiniMax OAuth state mismatch (possible CSRF).", "state_mismatch", false)
	}
	return payload, nil
}

// True when expired_in is plausibly an absolute unix millisecond time rather than TTL seconds
func minimaxExpiredInLooksLikeUnixMS(expiredIn, nowMS int64) bool { return expiredIn > nowMS/2 }

// Access token expiry as unix seconds from a millisecond epoch or a TTL in seconds
func minimaxResolveTokenExpiryUnix(expiredIn int64, now time.Time) float64 {
	if minimaxExpiredInLooksLikeUnixMS(expiredIn, now.UnixMilli()) {
		return float64(expiredIn) / 1000.0
	}
	return float64(now.UnixNano())/1e9 + float64(max(1, expiredIn))
}

// Obtained at, expires at and expires in fields derived from a MiniMax expired_in value
func minimaxExpiryFields(expiredIn any) (map[string]any, error) {
	raw, ok := pyInt(expiredIn)
	if !ok {
		return nil, fmt.Errorf("invalid literal for int(): %s", pyStr(expiredIn))
	}
	now := nowFunc().UTC()
	expiresAt := minimaxResolveTokenExpiryUnix(int64(raw), now)
	nowSeconds := float64(now.UnixNano()) / 1e9
	return map[string]any{
		"obtained_at": epochToISO(nowSeconds),
		"expires_at":  epochToISO(expiresAt),
		"expires_in":  max(0, int(expiresAt-nowSeconds)),
	}, nil
}

// Polls the MiniMax token endpoint until success, error or the code's deadline
func minimaxPollToken(ctx context.Context, client *http.Client, portalBaseURL, clientID, userCode, codeVerifier string, expiredIn int64, intervalMS *int) (map[string]any, error) {
	deadline := minimaxResolveTokenExpiryUnix(expiredIn, nowFunc().UTC())
	ms := 2000
	if intervalMS != nil && *intervalMS != 0 {
		ms = *intervalMS
	}
	interval := max(2.0, float64(ms)/1000.0)
	for nowEpoch() < deadline {
		resp, err := minimaxPostForm(ctx, client, portalBaseURL+"/oauth/token", url.Values{
			"grant_type": {MinimaxOAuthGrantType}, "client_id": {clientID}, "user_code": {userCode}, "code_verifier": {codeVerifier},
		}, formJSONHeaders)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			var errPayload map[string]any
			if resp.errorText != "" {
				_ = json.Unmarshal([]byte(resp.errorText), &errPayload)
			}
			msg := pyOrStr(anyMap(errPayload["base_resp"])["status_msg"])
			if msg == "" {
				msg = resp.errorText
			}
			if msg == "" {
				msg = "unknown"
			}
			return nil, minimaxErr("MiniMax OAuth error: "+msg, "token_exchange_failed", false)
		}
		var payload map[string]any
		if len(resp.Body) > 0 {
			_ = json.Unmarshal(resp.Body, &payload)
		}
		switch payload["status"] {
		case "error":
			return nil, minimaxErr("MiniMax OAuth reported an error. Please try again later.", "authorization_denied", false)
		case "success":
			for _, k := range []string{"access_token", "refresh_token", "expired_in"} {
				if !pyTruthy(payload[k]) {
					return nil, minimaxErr("MiniMax OAuth success payload missing required token fields.", "token_incomplete", false)
				}
			}
			return payload, nil
		}
		minimaxSleepFn(time.Duration(interval * float64(time.Second)))
	}
	return nil, minimaxErr("MiniMax OAuth timed out before authorization completed.", "timeout", false)
}

// Persists MiniMax OAuth state, making minimax-oauth the active provider only when asked
func minimaxSaveAuthState(env PoolEnv, authState map[string]any, setActive bool) error {
	unlock, err := env.Store.Lock(AuthLockTimeout)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := env.Store.Load()
	if err != nil {
		return err
	}
	if err := env.Store.StoreProviderState(data, "minimax-oauth", authState, setActive); err != nil {
		return err
	}
	return env.Store.Save(data)
}

// Runs the MiniMax OAuth user code flow, persists the tokens and returns the auth state
func minimaxOAuthLogin(env PoolEnv, out io.Writer, region string, openBrowser bool, timeout time.Duration) (map[string]any, error) {
	if region == "" {
		region = "global"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	portalBaseURL, inferenceBaseURL := MinimaxOAuthGlobalBase, MinimaxOAuthGlobalInference
	if region == "cn" {
		portalBaseURL, inferenceBaseURL = MinimaxOAuthCNBase, MinimaxOAuthCNInference
	}
	verifier, challenge, state := minimaxPKCEPair()
	if isRemoteSession() {
		openBrowser = false
	}
	fmt.Fprintf(out, "Starting Icebath login via MiniMax (%s) OAuth...\n", region)
	fmt.Fprintf(out, "Portal: %s\n", portalBaseURL)
	ctx := env.ctx()
	client := minimaxHTTPClientFn(timeout)
	codeData, err := minimaxRequestUserCode(ctx, client, portalBaseURL, MinimaxOAuthClientID, challenge, state)
	if err != nil {
		return nil, err
	}
	err = printDeviceCodeInstructions(out, pyStr(codeData["verification_uri"]), pyStr(codeData["user_code"]),
		openBrowser && canOpenGraphicalBrowser(), "", false)
	if err != nil {
		return nil, err
	}
	var intervalMS *int
	if raw := codeData["interval"]; raw != nil {
		v, ok := pyInt(raw)
		if !ok {
			return nil, fmt.Errorf("invalid literal for int(): %s", pyStr(raw))
		}
		intervalMS = &v
	}
	fmt.Fprintln(out, "Waiting for approval...")
	expiredIn, ok := pyInt(codeData["expired_in"])
	if !ok {
		return nil, fmt.Errorf("invalid literal for int(): %s", pyStr(codeData["expired_in"]))
	}
	tokenData, err := minimaxPollToken(ctx, client, portalBaseURL, MinimaxOAuthClientID, pyStr(codeData["user_code"]), verifier, int64(expiredIn), intervalMS)
	if err != nil {
		return nil, err
	}
	tokenType := any("Bearer")
	if v, has := tokenData["token_type"]; has {
		tokenType = v
	}
	authState := map[string]any{
		"provider": "minimax-oauth", "region": region, "portal_base_url": portalBaseURL, "inference_base_url": inferenceBaseURL,
		"client_id": MinimaxOAuthClientID, "scope": MinimaxOAuthScope, "token_type": tokenType,
		"access_token": tokenData["access_token"], "refresh_token": tokenData["refresh_token"], "resource_url": tokenData["resource_url"],
	}
	expiry, err := minimaxExpiryFields(tokenData["expired_in"])
	if err != nil {
		return nil, err
	}
	for k, v := range expiry {
		authState[k] = v
	}
	if err := minimaxSaveAuthState(env, authState, true); err != nil {
		return nil, err
	}
	fmt.Fprintln(out, "✓ MiniMax OAuth login successful.")
	if msg := pyOrStr(tokenData["notification_message"]); msg != "" {
		fmt.Fprintf(out, "Note from MiniMax: %s\n", msg)
	}
	return authState, nil
}

// Refreshes the MiniMax access token when forced or within the refresh skew of expiry
func refreshMinimaxOAuthState(env PoolEnv, state map[string]any, timeout time.Duration, force bool) (map[string]any, error) {
	if !pyTruthy(state["refresh_token"]) {
		return nil, minimaxErr("MiniMax OAuth state has no refresh_token; please re-login.", "no_refresh_token", true)
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	expiresAt, ok := ParseISOTimestamp(state["expires_at"])
	if !ok {
		expiresAt = 0
	}
	if !force && expiresAt-nowEpoch() > MinimaxOAuthRefreshSkew.Seconds() {
		return state, nil
	}
	resp, err := minimaxPostForm(env.ctx(), minimaxHTTPClientFn(timeout), pyStr(state["portal_base_url"])+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "client_id": {pyStr(state["client_id"])}, "refresh_token": {pyStr(state["refresh_token"])},
	}, formJSONHeaders)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		body := resp.errorText
		lower := strings.ToLower(body)
		relogin := false
		for _, marker := range []string{"invalid_grant", "refresh_token_reused", "invalid_refresh_token"} {
			relogin = relogin || strings.Contains(lower, marker)
		}
		if body == "" {
			body = resp.Reason
		}
		return nil, minimaxErr("MiniMax OAuth refresh failed: "+body, "refresh_failed", relogin)
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		return nil, err
	}
	if payload["status"] != "success" {
		return nil, minimaxErr("MiniMax OAuth refresh did not return success.", "refresh_failed", true)
	}
	newState := cloneMap(state)
	newState["access_token"] = payload["access_token"]
	if v, has := payload["refresh_token"]; has {
		newState["refresh_token"] = v
	}
	expiry, err := minimaxExpiryFields(payload["expired_in"])
	if err != nil {
		return nil, err
	}
	for k, v := range expiry {
		newState[k] = v
	}
	if err := minimaxSaveAuthState(env, newState, false); err != nil {
		return nil, err
	}
	return newState, nil
}

// Wipes dead MiniMax tokens from auth.json after a terminal refresh failure
func minimaxOAuthQuarantineOnTerminalRefresh(env PoolEnv, state map[string]any, authErr *AuthError) {
	if !(authErr.ReloginRequired && pyTruthy(state["refresh_token"])) {
		return
	}
	quarantineFlatOAuthState(state, "minimax-oauth", authErr)
	if err := minimaxSaveAuthState(env, state, false); err != nil {
		env.log().Debug(fmt.Sprintf("MiniMax OAuth: failed to persist quarantined state: %s", err))
	}
}

// MiniMax OAuth state refreshed near expiry under the store lock, quarantined on terminal failure
func minimaxFreshState(env PoolEnv) (map[string]any, error) {
	lockTimeout := AuthLockTimeout
	if lockTimeout < 20*time.Second {
		lockTimeout = 20 * time.Second
	}
	unlock, err := env.Store.Lock(lockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	state, err := env.Store.GetProviderAuthState("minimax-oauth")
	if err != nil {
		return nil, err
	}
	if len(state) == 0 || !pyTruthy(state["access_token"]) {
		return nil, minimaxErr("Not logged into MiniMax OAuth. Run `icebath model` and select MiniMax (OAuth).", "not_logged_in", true)
	}
	fresh, err := refreshMinimaxOAuthState(env, state, 15*time.Second, false)
	if err != nil {
		var authErr *AuthError
		if errors.As(err, &authErr) {
			minimaxOAuthQuarantineOnTerminalRefresh(env, state, authErr)
		}
		return nil, err
	}
	return fresh, nil
}

// BuildMinimaxOAuthTokenProvider mints a fresh MiniMax access token per call, tagged as the MiniMax OAuth source
func BuildMinimaxOAuthTokenProvider(env PoolEnv) *TaggedTokenSource {
	return TagCredentialSource(func(ctx context.Context) (string, error) {
		if ctx != nil {
			env.Ctx = ctx
		}
		state, err := minimaxFreshState(env)
		if err != nil {
			return "", err
		}
		token := pyOrStr(state["access_token"])
		if token == "" {
			return "", minimaxErr("MiniMax OAuth state has no access_token after refresh.", "no_access_token", true)
		}
		return token, nil
	}, CredentialSourceMinimaxOAuth)
}

// Runtime MiniMax OAuth credentials with a string bearer, or a refreshing token source when asked
func resolveMinimaxOAuthRuntimeCredentialsWith(env PoolEnv, asTokenProvider bool) (map[string]any, error) {
	state, err := minimaxFreshState(env)
	if err != nil {
		return nil, err
	}
	var apiKey any = state["access_token"]
	if asTokenProvider {
		apiKey = BuildMinimaxOAuthTokenProvider(env)
	}
	return map[string]any{
		"provider": "minimax-oauth", "api_key": apiKey, "base_url": strings.TrimRight(pyStr(state["inference_base_url"]), "/"), "source": "oauth",
	}, nil
}

// Runtime MiniMax OAuth credentials with the access token as a string
func resolveMinimaxOAuthRuntimeCredentials(env PoolEnv) (map[string]any, error) {
	return resolveMinimaxOAuthRuntimeCredentialsWith(env, false)
}

// MinimaxLoginArgs are the MiniMax login flags
type MinimaxLoginArgs struct {
	Region    string
	NoBrowser bool
	Timeout   time.Duration
}

// LoginMinimaxOAuth runs the MiniMax OAuth login, printing the formatted auth error and exiting 1 on failure
func LoginMinimaxOAuth(env PoolEnv, out io.Writer, args MinimaxLoginArgs) error {
	_, err := minimaxOAuthLogin(env, out, args.Region, !args.NoBrowser, args.Timeout)
	var authErr *AuthError
	if errors.As(err, &authErr) {
		fmt.Fprintln(out, formatAuthError(authErr))
		return &OAuthLoginExitError{Code: 1, Err: err}
	}
	return err
}
