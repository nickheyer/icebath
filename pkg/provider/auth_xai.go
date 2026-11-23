package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const xaiRelogin = "Re-authenticate with `icebath model`."

// Client for the xAI OIDC discovery, device code and token endpoints, swappable so tests script xAI
var xaiHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Pure refresh the runtime resolver goes through, swappable so tests script the token endpoint
var xaiRefreshPureAtFn = refreshXAIOAuthPureAt

// Device code login the xAI login command runs, swappable so tests script the whole browser flow
var xaiOAuthDeviceCodeLoginFn = xaiOAuthDeviceCodeLogin

func xaiClean(v any) string { return strings.TrimSpace(pyOrStr(v)) }

// Stripped access and refresh tokens, empty strings when the value is not a mapping
func xaiTokenPair(tokens any) (string, string) {
	m, ok := tokens.(map[string]any)
	if !ok {
		return "", ""
	}
	return xaiClean(m["access_token"]), xaiClean(m["refresh_token"])
}

// xaiSaveOptions carries the optional parts of an xAI token save
type xaiSaveOptions struct {
	Discovery   map[string]any
	RedirectURI string
	LastRefresh string
	AuthMode    string
	SetActive   bool
}

func isXAIOriginHost(host string) bool { return host == "x.ai" || strings.HasSuffix(host, ".x.ai") }

// Problem with a URL as scheme, host, origin or empty when it is HTTPS on x.ai, and its lowercased host
func xaiURLProblem(raw string) (string, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(parsed.Hostname())
	switch {
	case parsed.Scheme != "https":
		return "scheme", host, nil
	case host == "":
		return "host", host, nil
	case !isXAIOriginHost(host):
		return "origin", host, nil
	}
	return "", host, nil
}

// Refuses a discovery endpoint that is not HTTPS on the xAI origin
func xaiValidateOAuthEndpoint(raw, field string) (string, error) {
	problem, host, err := xaiURLProblem(raw)
	if err != nil {
		return "", err
	}
	switch problem {
	case "":
		return raw, nil
	case "scheme":
		return "", xaiErr(fmt.Sprintf("xAI OIDC discovery returned a non-HTTPS %s: %s.", field, pyStringRepr(raw)), "xai_discovery_invalid", false)
	case "host":
		return "", xaiErr(fmt.Sprintf("xAI OIDC discovery %s is missing a hostname: %s.", field, pyStringRepr(raw)), "xai_discovery_invalid", false)
	}
	return "", xaiErr(fmt.Sprintf("xAI OIDC discovery %s host %s is not on the xAI origin "+
		"(expected x.ai or a *.x.ai subdomain). Refusing to use a cached "+
		"endpoint that may have been substituted by a MITM during initial "+
		"discovery; re-authenticate with `icebath model` to re-fetch.", field, pyStringRepr(host)), "xai_discovery_invalid", false)
}

// Pins the OAuth inference base URL to the xAI origin, warning and using the fallback on rejection
func xaiValidateInferenceBaseURL(env PoolEnv, value, fallback string) string {
	candidate := strings.TrimRight(strings.TrimSpace(value), "/")
	if candidate == "" {
		return fallback
	}
	problem, host, err := xaiURLProblem(candidate)
	if err != nil {
		env.log().Warn(fmt.Sprintf("Ignoring malformed xAI base_url override %s; using %s instead.", pyStringRepr(candidate), fallback))
		return fallback
	}
	switch problem {
	case "":
		return candidate
	case "scheme":
		env.log().Warn(fmt.Sprintf("Refusing non-HTTPS xAI base_url override %s (xai-oauth bearer would "+
			"be sent in cleartext); falling back to %s.", pyStringRepr(candidate), fallback))
	case "host":
		env.log().Warn(fmt.Sprintf("Ignoring xAI base_url override %s with no hostname; using %s instead.", pyStringRepr(candidate), fallback))
	default:
		env.log().Warn(fmt.Sprintf("Refusing xAI base_url override %s — host %s is not on the xAI origin "+
			"(expected x.ai or a *.x.ai subdomain). The xai-oauth bearer is only "+
			"valid against xAI's inference API; sending it elsewhere would leak "+
			"the credential. Falling back to %s.", pyStringRepr(candidate), pyStringRepr(host), fallback))
	}
	return fallback
}

// Fetches and validates the xAI OIDC authorization and token endpoints
func xaiOAuthDiscovery(ctx context.Context, timeout time.Duration) (map[string]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, XAIOAuthDiscoveryURL, nil)
	if err != nil {
		return nil, xaiErr(fmt.Sprintf("xAI OIDC discovery failed: %s", err), "xai_discovery_failed", false)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := oauthDo(xaiHTTPClientFn(timeout), req, 0, nil)
	if err != nil {
		ae := xaiErr(fmt.Sprintf("xAI OIDC discovery failed: %s", err), "xai_discovery_failed", false)
		ae.Err = err
		return nil, ae
	}
	if resp.Status != 200 {
		return nil, xaiErr(fmt.Sprintf("xAI OIDC discovery returned status %d.", resp.Status), "xai_discovery_failed", false)
	}
	raw, err := resp.JSON()
	if err != nil {
		ae := xaiErr(fmt.Sprintf("xAI OIDC discovery returned invalid JSON: %s", err), "xai_discovery_invalid_json", false)
		ae.Err = err
		return nil, ae
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return nil, xaiErr("xAI OIDC discovery response was not a JSON object.", "xai_discovery_incomplete", false)
	}
	endpoints := map[string]string{}
	for _, k := range []string{"authorization_endpoint", "token_endpoint"} {
		endpoints[k] = xaiClean(payload[k])
		if endpoints[k] == "" {
			return nil, xaiErr("xAI OIDC discovery response was missing required endpoints.", "xai_discovery_incomplete", false)
		}
	}
	for _, field := range []string{"authorization_endpoint", "token_endpoint"} {
		if _, err := xaiValidateOAuthEndpoint(endpoints[field], field); err != nil {
			return nil, err
		}
	}
	return endpoints, nil
}

// Token block persisted for xAI OAuth, keeping the prior refresh token when none rotated in
func xaiTokensFromPayload(payload map[string]any, accessToken, fallbackRefresh string) map[string]any {
	refresh := pyOrStr(payload["refresh_token"])
	if refresh == "" {
		refresh = fallbackRefresh
	}
	tokenType := xaiClean(payload["token_type"])
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return map[string]any{
		"access_token": accessToken, "refresh_token": strings.TrimSpace(refresh),
		"id_token": xaiClean(payload["id_token"]), "expires_in": payload["expires_in"], "token_type": tokenType,
	}
}

// Refreshes an xAI grant through OIDC discovery, returning the rotated token block and refresh stamp
func refreshXAIOAuthPure(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
	return xaiRefreshPureAtFn(ctx, accessToken, refreshToken, "", timeout)
}

// Refreshes an xAI grant against the given token endpoint, discovering it when empty
func refreshXAIOAuthPureAt(ctx context.Context, accessToken, refreshToken, tokenEndpoint string, timeout time.Duration) (map[string]any, error) {
	_ = accessToken
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, xaiErr("xAI OAuth is missing refresh_token. "+xaiRelogin, "xai_auth_missing_refresh_token", true)
	}
	endpoint := strings.TrimSpace(tokenEndpoint)
	if endpoint == "" {
		discovery, err := xaiOAuthDiscovery(ctx, timeout)
		if err != nil {
			return nil, err
		}
		endpoint = discovery["token_endpoint"]
	}
	if _, err := xaiValidateOAuthEndpoint(endpoint, "token_endpoint"); err != nil {
		return nil, err
	}
	if timeout < 5*time.Second {
		timeout = 5 * time.Second
	}
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {XAIOAuthClientID}, "refresh_token": {refreshToken}}
	resp, err := oauthPostForm(ctx, xaiHTTPClientFn(timeout), endpoint, form, map[string]string{"Accept": "application/json"})
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		detail := strings.TrimSpace(string(resp.Body))
		suffix := ""
		if detail != "" {
			suffix = " Response: " + detail
		}
		if resp.Status == 403 {
			return nil, xaiErr("xAI token refresh failed with HTTP 403."+suffix+
				" This OAuth account is not authorized for xAI API"+
				" access — xAI may be restricting API/OAuth use to"+
				" specific SuperGrok tiers despite the in-app"+
				" subscription being active. Re-logging in won't"+
				" change that; set ``XAI_API_KEY`` and switch to"+
				" ``provider: xai`` (API-key path) if available, or"+
				" upgrade your subscription at https://x.ai/grok.", "xai_oauth_tier_denied", false)
		}
		return nil, xaiErr("xAI token refresh failed."+suffix, "xai_refresh_failed", resp.Status == 400 || resp.Status == 401)
	}
	noRelogin := false
	payload, refreshedAccess, err := oauthRefreshPayloadAccessToken(resp, oauthRefreshParse{
		provider:           "xai-oauth",
		invalidJSON:        [2]string{"xAI token refresh returned invalid JSON: {exc}", "xai_refresh_invalid_json"},
		invalidJSONRelogin: &noRelogin,
		invalidResponse:    &[2]string{"xAI token refresh response was not a JSON object.", "xai_refresh_invalid_response"},
		missingAccess:      [2]string{"xAI token refresh response was missing access_token.", "xai_refresh_missing_access_token"},
		reloginRequired:    true,
	})
	if err != nil {
		return nil, err
	}
	out := xaiTokensFromPayload(payload, refreshedAccess, refreshToken)
	out["last_refresh"] = utcNowZ()
	return out, nil
}

// Inference base URL for the xAI OAuth bearer, env overrides pinned to the xAI origin
func xaiOAuthInferenceBaseURL(env PoolEnv) string {
	value := strings.TrimRight(strings.TrimSpace(GetEnvPreferDotenv(env, "ICEBATH_XAI_BASE_URL")), "/")
	if value == "" {
		value = strings.TrimRight(strings.TrimSpace(GetEnvPreferDotenv(env, "XAI_BASE_URL")), "/")
	}
	return xaiValidateInferenceBaseURL(env, value, DefaultXAIOAuthBaseURL)
}

// Runtime xAI OAuth credentials of the selected xAI login, refreshed through the pool when forced or close to expiry
func resolveXAIOAuthRuntimeCredentials(env PoolEnv, forceRefresh, refreshIfExpiring bool, refreshSkew time.Duration) (map[string]any, error) {
	pool := LoadSelectedPool(env, "xai-oauth")
	entry, ok := pool.Peek()
	if !ok {
		return nil, xaiErr("No xAI login. Sign in with `icebath auth add xai-oauth`.", "xai_auth_missing", true)
	}
	token := entry.RuntimeAPIKey()
	skew := refreshSkew
	if skew == 0 {
		skew = xaiProactiveRefreshSkew(token)
	}
	if forceRefresh || (refreshIfExpiring && xaiAccessTokenIsExpiring(token, skew)) {
		refreshed, ok := pool.TryRefreshMatching(token, entry.ID)
		if !ok {
			return nil, xaiErr("The xAI login could not be refreshed. "+xaiRelogin, "xai_refresh_failed", true)
		}
		entry, token = refreshed, refreshed.RuntimeAPIKey()
	}
	return map[string]any{
		"provider": "xai-oauth", "base_url": xaiOAuthInferenceBaseURL(env), "api_key": token,
		"source": "credential_pool", "last_refresh": entry.LastRefresh, "auth_mode": "oauth_device_code",
	}, nil
}

// Exported xAI OAuth runtime credential resolution for the agent client refresh
func ResolveXAIOAuthRuntimeCredentials(env PoolEnv, forceRefresh, refreshIfExpiring bool, refreshSkew time.Duration) (map[string]any, error) {
	return resolveXAIOAuthRuntimeCredentials(env, forceRefresh, refreshIfExpiring, refreshSkew)
}

// XAILoginArgs are the xAI login flags
type XAILoginArgs struct {
	Timeout   time.Duration
	NoBrowser bool
}

// LoginXAIOAuth signs in to xAI Grok OAuth through the device code flow, stored as an owned login made the default, and makes it the provider
func LoginXAIOAuth(env PoolEnv, out io.Writer, args XAILoginArgs, forceNewLogin bool) error {
	if !forceNewLogin {
		accepted, err := offerExistingOAuthCredentials(env, out, "xai-oauth",
			func() (map[string]any, error) { return resolveXAIOAuthRuntimeCredentials(env, false, true, 0) },
			xaiAccessTokenIsExpiring, "xAI OAuth", DefaultXAIOAuthBaseURL, "")
		if err != nil {
			return err
		}
		if accepted {
			return nil
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...")
	fmt.Fprintln(out, "(Icebath creates its own local OAuth session)")
	fmt.Fprintln(out)
	login, err := SignInOAuthLogin(env, out, "xai-oauth", OAuthSignIn{NoBrowser: args.NoBrowser, Timeout: args.Timeout, MakeDefault: true})
	if err != nil {
		return err
	}
	configPath, err := updateConfigForProvider(env, "xai-oauth", DefaultXAIOAuthBaseURL, "", false)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Signed in as the login %s.\n", login.Name)
	printLoginSuccess(out, envHome(env), "xai-oauth", configPath, true)
	return nil
}

// Requests an xAI device code, every RFC 8628 field required
func xaiOAuthRequestDeviceCode(ctx context.Context, client *http.Client, scope string) (map[string]any, error) {
	if scope == "" {
		scope = XAIOAuthScope
	}
	resp, err := oauthPostForm(ctx, client, XAIOAuthDeviceCodeURL, url.Values{"client_id": {XAIOAuthClientID}, "scope": {scope}}, formJSONHeaders)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		detail := ""
		if len(resp.Body) > 0 {
			detail = " Response: " + strings.TrimSpace(string(resp.Body))
		}
		return nil, xaiErr(fmt.Sprintf("xAI device-code request failed (HTTP %d).%s", resp.Status, detail), "device_code_request_failed", false)
	}
	payload, err := resp.JSONObject()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, key := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if _, has := payload[key]; !has {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, xaiErr("xAI device-code response missing fields: "+strings.Join(missing, ", "), "device_code_invalid", false)
	}
	return payload, nil
}

// Polls the xAI token endpoint until the device code is approved, declined, expired or the context ends
func xaiOAuthPollDeviceToken(ctx context.Context, client *http.Client, tokenEndpoint, deviceCode string, expiresIn, pollInterval int) (map[string]any, error) {
	if pollInterval < 1 {
		pollInterval = 1
	}
	payload, err := pollDeviceTokenGeneric(deviceTokenPoll{
		post: func() (*oauthHTTPResponse, error) {
			return oauthPostForm(ctx, client, tokenEndpoint, url.Values{
				"grant_type": {DeviceCodeGrantType}, "client_id": {XAIOAuthClientID}, "device_code": {deviceCode}}, formJSONHeaders)
		},
		expiresIn:    expiresIn,
		pollInterval: pollInterval,
		validateSuccess: func(payload map[string]any) error {
			for _, f := range [][2]string{{"access_token", "an"}, {"refresh_token", "a"}} {
				if !pyTruthy(payload[f[0]]) {
					return xaiErr(fmt.Sprintf("xAI device-code token response did not include %s %s.", f[1], f[0]), "xai_device_token_invalid", false)
				}
			}
			return nil
		},
		onNonJSONError: func(*oauthHTTPResponse) error {
			return xaiErr("xAI device-code token polling returned a non-JSON error response.", "xai_device_token_failed", false)
		},
		onError: func(resp *oauthHTTPResponse, payload map[string]any) error {
			description := pyOrStr(payload["error_description"])
			if description == "" {
				description = pyOrStr(payload["error"])
			}
			if description == "" {
				description = string(resp.Body)
			}
			ae := xaiErr("xAI device-code token polling failed: "+description, "xai_device_token_failed", false)
			ae.Err = &SignInCopyError{Message: description, OAuthErrorCode: pyOrStr(payload["error"])}
			return ae
		},
		onTimeout: func(error) error {
			return xaiErr("Timed out waiting for xAI device authorization.", "device_code_timeout", false)
		},
		cancelled: ctxCancelled(ctx),
	})
	if err != nil && ctx.Err() != nil {
		return nil, &SignInCancelledError{ExitCode: 130, HasCode: true}
	}
	return payload, err
}

// Runs the xAI device code login and returns the token block, discovery and inference base URL
func xaiOAuthDeviceCodeLogin(env PoolEnv, out io.Writer, timeout time.Duration, openBrowser bool) (map[string]any, error) {
	ctx := env.ctx()
	discovery, err := xaiOAuthDiscovery(ctx, timeout)
	if err != nil {
		return nil, err
	}
	clientTimeout := timeout
	if clientTimeout < 20*time.Second {
		clientTimeout = 20 * time.Second
	}
	client := xaiHTTPClientFn(clientTimeout)
	deviceData, err := xaiOAuthRequestDeviceCode(ctx, client, XAIOAuthScope)
	if err != nil {
		return nil, err
	}
	interval, ok := pyInt(deviceData["interval"])
	if !ok {
		return nil, fmt.Errorf("the device code interval %s is not a whole number", pyStr(deviceData["interval"]))
	}
	expiresIn, ok := pyInt(deviceData["expires_in"])
	if !ok {
		return nil, fmt.Errorf("the device code lifetime %s is not a whole number", pyStr(deviceData["expires_in"]))
	}
	verificationURL := pyOrStr(deviceData["verification_uri_complete"])
	if verificationURL == "" {
		verificationURL = pyStr(deviceData["verification_uri"])
	}
	err = printDeviceCodeInstructions(out, verificationURL, pyStr(deviceData["user_code"]),
		openBrowser && !isRemoteSession() && canOpenGraphicalBrowser(), "", true)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "Waiting for approval (polling every %ds)...\n", max(1, interval))
	payload, err := xaiOAuthPollDeviceToken(ctx, client, discovery["token_endpoint"], pyStr(deviceData["device_code"]), expiresIn, interval)
	if err != nil {
		return nil, err
	}
	accessToken, refreshToken := xaiTokenPair(payload)
	if accessToken == "" || refreshToken == "" {
		return nil, xaiErr("xAI device-code token response was missing required tokens.", "xai_device_token_invalid", false)
	}
	return map[string]any{
		"tokens":       xaiTokensFromPayload(payload, accessToken, refreshToken),
		"discovery":    map[string]any{"authorization_endpoint": discovery["authorization_endpoint"], "token_endpoint": discovery["token_endpoint"]},
		"redirect_uri": "", "base_url": xaiOAuthInferenceBaseURL(env), "last_refresh": utcNowZ(), "source": "oauth-device-code",
	}, nil
}
