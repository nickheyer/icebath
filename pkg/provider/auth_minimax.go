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
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const minimaxOAuthErrorBodyLimit = 16 * 1024

// Client for the MiniMax OAuth endpoints, swappable so tests aim it at a local server
var minimaxHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

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
	out := pystr.DecodeUTF8Replace(text)
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
		return nil, fmt.Errorf("the MiniMax token lifetime %s is not a whole number", pyStr(expiredIn))
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

// Polls the MiniMax token endpoint until success, error, the code's deadline or the context ends
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
			if ctx.Err() != nil {
				return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
			}
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
		if err := cancellableSleep(time.Duration(interval*float64(time.Second)), ctxCancelled(ctx)); err != nil {
			return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
	}
	return nil, minimaxErr("MiniMax OAuth timed out before authorization completed.", "timeout", false)
}

// Runs the MiniMax OAuth user code flow, persists the tokens and returns the auth state
func minimaxOAuthLogin(env PoolEnv, out io.Writer, region string, openBrowser bool, timeout time.Duration) (map[string]any, error) {
	if region == "" {
		region = "global"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	pconfig, err := providerConfig(env.ctx(), "minimax-oauth")
	if err != nil {
		return nil, err
	}
	portalBaseURL, inferenceBaseURL := pconfig.PortalBaseURL, pconfig.BaseURL
	if region == "cn" {
		cnPortal, ok := pconfig.Extra["cn_portal_base_url"].(string)
		if !ok {
			return nil, errors.New("the MiniMax region config has no cn_portal_base_url")
		}
		cnInference, ok := pconfig.Extra["cn_inference_base_url"].(string)
		if !ok {
			return nil, errors.New("the MiniMax region config has no cn_inference_base_url")
		}
		portalBaseURL, inferenceBaseURL = cnPortal, cnInference
	}
	verifier, challenge, state := minimaxPKCEPair()
	if isRemoteSession() {
		openBrowser = false
	}
	fmt.Fprintf(out, "Starting Icebath login via MiniMax (%s) OAuth...\n", region)
	fmt.Fprintf(out, "Portal: %s\n", portalBaseURL)
	ctx := env.ctx()
	client := minimaxHTTPClientFn(timeout)
	codeData, err := minimaxRequestUserCode(ctx, client, portalBaseURL, pconfig.ClientID, challenge, state)
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
			return nil, fmt.Errorf("the MiniMax device code value %s is not a whole number", pyStr(raw))
		}
		intervalMS = &v
	}
	fmt.Fprintln(out, "Waiting for approval...")
	expiredIn, ok := pyInt(codeData["expired_in"])
	if !ok {
		return nil, fmt.Errorf("the MiniMax device code lifetime %s is not a whole number", pyStr(codeData["expired_in"]))
	}
	tokenData, err := minimaxPollToken(ctx, client, portalBaseURL, pconfig.ClientID, pyStr(codeData["user_code"]), verifier, int64(expiredIn), intervalMS)
	if err != nil {
		return nil, err
	}
	tokenType := any("Bearer")
	if v, has := tokenData["token_type"]; has {
		tokenType = v
	}
	authState := map[string]any{
		"provider": "minimax-oauth", "region": region, "portal_base_url": portalBaseURL, "inference_base_url": inferenceBaseURL,
		"client_id": pconfig.ClientID, "scope": MinimaxOAuthScope, "token_type": tokenType,
		"access_token": tokenData["access_token"], "refresh_token": tokenData["refresh_token"], "resource_url": tokenData["resource_url"],
	}
	expiry, err := minimaxExpiryFields(tokenData["expired_in"])
	if err != nil {
		return nil, err
	}
	for k, v := range expiry {
		authState[k] = v
	}
	fmt.Fprintln(out, "✓ MiniMax OAuth login successful.")
	if msg := pyOrStr(tokenData["notification_message"]); msg != "" {
		fmt.Fprintf(out, "Note from MiniMax: %s\n", msg)
	}
	return authState, nil
}

// Selected MiniMax login, refreshed through its pool when close to expiry
func minimaxLoginEntry(env PoolEnv) (PooledCredential, error) {
	pool := LoadSelectedPool(env, "minimax-oauth")
	entry, ok := pool.Peek()
	if !ok {
		return PooledCredential{}, minimaxErr("No MiniMax login. Sign in with `icebath auth add minimax-oauth`.", "not_logged_in", true)
	}
	if pool.entryNeedsRefresh(entry) {
		refreshed, ok := pool.TryRefreshMatching(entry.AccessToken, entry.ID)
		if !ok {
			return PooledCredential{}, minimaxErr("The MiniMax login could not be refreshed; sign in again.", "refresh_failed", true)
		}
		entry = refreshed
	}
	return entry, nil
}

// BuildMinimaxOAuthTokenProvider mints a fresh MiniMax access token per call, tagged as the MiniMax OAuth source
func BuildMinimaxOAuthTokenProvider(env PoolEnv) *TaggedTokenSource {
	return TagCredentialSource(func(ctx context.Context) (string, error) {
		if ctx != nil {
			env.Ctx = ctx
		}
		entry, err := minimaxLoginEntry(env)
		if err != nil {
			return "", err
		}
		if entry.AccessToken == "" {
			return "", minimaxErr("The MiniMax login has no access token.", "no_access_token", true)
		}
		return entry.AccessToken, nil
	}, CredentialSourceMinimaxOAuth)
}

// Runtime MiniMax OAuth credentials with a string bearer, or a refreshing token source when asked
func resolveMinimaxOAuthRuntimeCredentialsWith(env PoolEnv, asTokenProvider bool) (map[string]any, error) {
	entry, err := minimaxLoginEntry(env)
	if err != nil {
		return nil, err
	}
	var apiKey any = entry.AccessToken
	if asTokenProvider {
		apiKey = BuildMinimaxOAuthTokenProvider(env)
	}
	return map[string]any{
		"provider": "minimax-oauth", "api_key": apiKey, "base_url": strings.TrimRight(entry.BaseURL, "/"), "source": "credential_pool",
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

// LoginMinimaxOAuth runs the MiniMax OAuth sign in stored as an owned login made the default, printing the formatted auth error and exiting 1 on failure
func LoginMinimaxOAuth(env PoolEnv, out io.Writer, args MinimaxLoginArgs) error {
	_, err := SignInOAuthLogin(env, out, "minimax-oauth", OAuthSignIn{NoBrowser: args.NoBrowser, Region: args.Region, Timeout: args.Timeout, MakeDefault: true})
	var authErr *AuthError
	if errors.As(err, &authErr) {
		fmt.Fprintln(out, formatAuthError(authErr))
		return &OAuthLoginExitError{Code: 1, Err: err}
	}
	return err
}

// Token POST of a MiniMax login row at the portal and client id its sign in stored on the row
func refreshMinimaxLogin(ctx context.Context, entry PooledCredential, timeout time.Duration) (map[string]any, error) {
	if entry.RefreshToken == "" {
		return nil, minimaxErr("The MiniMax login has no refresh token; sign in again.", "no_refresh_token", true)
	}
	resp, err := minimaxPostForm(ctx, minimaxHTTPClientFn(timeout), entry.ExtraString("portal_base_url")+"/oauth/token", url.Values{
		"grant_type": {"refresh_token"}, "client_id": {entry.ExtraString("client_id")}, "refresh_token": {entry.RefreshToken},
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
	expiry, err := minimaxExpiryFields(payload["expired_in"])
	if err != nil {
		return nil, err
	}
	refresh := pyOrStr(payload["refresh_token"])
	if refresh == "" {
		refresh = entry.RefreshToken
	}
	out := map[string]any{"access_token": pyOrStr(payload["access_token"]), "refresh_token": refresh}
	if epoch, ok := ParseISOTimestamp(expiry["expires_at"]); ok {
		out["expires_at_ms"] = epoch * 1000
	}
	return out, nil
}

// Whether a failed MiniMax refresh ended the grant so only a new sign in helps
func isTerminalMinimaxOAuthRefreshError(err error) bool {
	var authErr *AuthError
	return errors.As(err, &authErr) && authErr.ReloginRequired
}
