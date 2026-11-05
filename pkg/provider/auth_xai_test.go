package provider

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestXAIOAuthTokenExpiringUsesOneHourSkew(t *testing.T) {
	token := jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 30*60})
	if !xaiAccessTokenIsExpiring(token, XAIAccessTokenRefreshSkew) {
		t.Fatal("a token 30 minutes from expiry is expiring under the one hour skew")
	}
}

func TestXAIProactiveRefreshSkewShortLivedToken(t *testing.T) {
	token := jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 15*60})
	skew := xaiProactiveRefreshSkew(token)
	if skew != 120*time.Second || xaiAccessTokenIsExpiring(token, skew) {
		t.Fatalf("skew %v", skew)
	}
}

func TestXAIReadTokensUsesCredentialPoolWhenProviderTokensEmpty(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{
		"providers": map[string]any{"xai-oauth": map[string]any{"tokens": map[string]any{}, "last_auth_error": map[string]any{}}},
		"credential_pool": map[string]any{"xai-oauth": []any{map[string]any{
			"access_token": "pool-access", "refresh_token": "pool-refresh", "token_type": "Bearer", "last_refresh": "2026-06-03T19:00:00Z"}}},
	})
	resolved, err := readXAIOAuthTokens(testEnv(t, h), false)
	if err != nil {
		t.Fatal(err)
	}
	tokens := resolved["tokens"].(map[string]any)
	if tokens["access_token"] != "pool-access" || tokens["refresh_token"] != "pool-refresh" || tokens["token_type"] != "Bearer" ||
		resolved["last_refresh"] != "2026-06-03T19:00:00Z" {
		t.Fatalf("%v", resolved)
	}
}

func TestXAIReadTokensMissingIsRelogin(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	_, err := readXAIOAuthTokens(testEnv(t, h), true)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "xai_auth_missing" || !ae.ReloginRequired {
		t.Fatalf("%v", err)
	}
}

func TestXAIWriteThroughIsNoopInClassicMode(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if err := saveXAIOAuthTokens(testEnv(t, h), map[string]any{"access_token": "a", "refresh_token": "r"}, xaiSaveOptions{SetActive: true}); err != nil {
		t.Fatal(err)
	}
	state := readAuthStore(t, h)["providers"].(map[string]any)["xai-oauth"].(map[string]any)
	if state["tokens"].(map[string]any)["refresh_token"] != "r" || state["auth_mode"] != "oauth_device_code" {
		t.Fatalf("%v", state)
	}
}

func TestXAIRootResolvedGrantWritesBackToRootOnly(t *testing.T) {
	profile, root, env := profileEnvForTest(t, "work")
	writeAuthStore(t, profile, map[string]any{"version": 1, "providers": map[string]any{}})
	writeAuthStore(t, root, map[string]any{"version": 1, "providers": map[string]any{"xai-oauth": map[string]any{
		"tokens": map[string]any{"access_token": "old", "refresh_token": "old-r"}}}})
	if err := saveXAIOAuthTokens(env, map[string]any{"access_token": "new", "refresh_token": "new-r"}, xaiSaveOptions{}); err != nil {
		t.Fatal(err)
	}
	rootState := readAuthStore(t, root)["providers"].(map[string]any)["xai-oauth"].(map[string]any)
	if rootState["tokens"].(map[string]any)["refresh_token"] != "new-r" {
		t.Fatalf("root %v", rootState)
	}
	if _, has := readAuthStore(t, profile)["providers"].(map[string]any)["xai-oauth"]; has {
		t.Fatal("a profile copy would shadow root")
	}
}

func TestXAIValidateOAuthEndpointPinsTheOrigin(t *testing.T) {
	for raw, code := range map[string]string{
		"https://auth.x.ai/oauth2/token": "", "http://auth.x.ai/oauth2/token": "xai_discovery_invalid",
		"https://evil.example/token": "xai_discovery_invalid", "https:///token": "xai_discovery_invalid", "https://notx.ai/t": "xai_discovery_invalid",
	} {
		_, err := xaiValidateOAuthEndpoint(raw, "token_endpoint")
		var ae *AuthError
		if code == "" && err != nil || code != "" && (!errors.As(err, &ae) || ae.Code != code) {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	h := newTestHome(t)
	env := testEnv(t, h)
	t.Setenv("ICEBATH_XAI_BASE_URL", "http://api.x.ai/v1")
	if got := xaiOAuthInferenceBaseURL(env); got != DefaultXAIOAuthBaseURL {
		t.Fatal(got)
	}
	t.Setenv("ICEBATH_XAI_BASE_URL", "https://us-east.api.x.ai/v1/")
	if got := xaiOAuthInferenceBaseURL(env); got != "https://us-east.api.x.ai/v1" {
		t.Fatal(got)
	}
}

func TestXAIRefreshPureStatusCodes(t *testing.T) {
	status, body := 200, `{"access_token":"fresh","refresh_token":"rot","expires_in":3600}`
	scriptClient(t, &xaiHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		form := readForm(t, req)
		if req.URL.String() != "https://auth.x.ai/oauth2/token" || form["grant_type"] != "refresh_token" || form["refresh_token"] != "r0" || form["client_id"] != XAIOAuthClientID {
			t.Errorf("request %s %v", req.URL, form)
		}
		return textResponse(status, body), nil
	})
	out, err := refreshXAIOAuthPureAt(nil, "a0", "r0", "https://auth.x.ai/oauth2/token", time.Second)
	if err != nil || out["access_token"] != "fresh" || out["refresh_token"] != "rot" || out["token_type"] != "Bearer" || out["last_refresh"] == "" {
		t.Fatalf("%v %v", out, err)
	}
	status, body = 403, "denied"
	_, err = refreshXAIOAuthPureAt(nil, "a0", "r0", "https://auth.x.ai/oauth2/token", time.Second)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "xai_oauth_tier_denied" || ae.ReloginRequired || !strings.Contains(ae.Message, "Response: denied") || isTerminalXAIOAuthRefreshError(err) {
		t.Fatalf("%v", err)
	}
	status, body = 401, ""
	_, err = refreshXAIOAuthPureAt(nil, "a0", "r0", "https://auth.x.ai/oauth2/token", time.Second)
	if !errors.As(err, &ae) || ae.Code != "xai_refresh_failed" || !ae.ReloginRequired || ae.Message != "xAI token refresh failed." || !isTerminalXAIOAuthRefreshError(err) {
		t.Fatalf("%v", err)
	}
	status, body = 200, "[1]"
	_, err = refreshXAIOAuthPureAt(nil, "a0", "r0", "https://auth.x.ai/oauth2/token", time.Second)
	if !errors.As(err, &ae) || ae.Code != "xai_refresh_invalid_response" {
		t.Fatalf("%v", err)
	}
	_, err = refreshXAIOAuthPureAt(nil, "a0", " ", "https://auth.x.ai/oauth2/token", time.Second)
	if !errors.As(err, &ae) || ae.Code != "xai_auth_missing_refresh_token" || !isTerminalXAIOAuthRefreshError(err) {
		t.Fatalf("%v", err)
	}
	_, err = refreshXAIOAuthPureAt(nil, "a0", "r0", "https://evil.example/token", time.Second)
	if !errors.As(err, &ae) || ae.Code != "xai_discovery_invalid" {
		t.Fatalf("%v", err)
	}
}

func TestXAIResolveRefreshesExpiringGrantAndQuarantinesDeadOne(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	expiring := jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 60})
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"xai-oauth": map[string]any{
		"tokens": map[string]any{"access_token": expiring, "refresh_token": "r0"}, "auth_mode": "oauth_pkce",
		"discovery": map[string]any{"token_endpoint": "https://auth.x.ai/oauth2/token"}}}})
	status, body := 200, `{"access_token":"fresh","refresh_token":"r1"}`
	scriptClient(t, &xaiHTTPClientFn, func(*http.Request) (*http.Response, error) { return textResponse(status, body), nil })
	creds, err := resolveXAIOAuthRuntimeCredentials(env, false, true, 0)
	if err != nil || creds["api_key"] != "fresh" || creds["source"] != "icebath-auth-store" || creds["base_url"] != DefaultXAIOAuthBaseURL {
		t.Fatalf("%v %v", creds, err)
	}
	state := readAuthStore(t, h)["providers"].(map[string]any)["xai-oauth"].(map[string]any)
	if state["tokens"].(map[string]any)["refresh_token"] != "r1" || state["auth_mode"] != "oauth_pkce" {
		t.Fatalf("%v", state)
	}
	status, body = 400, `{"error":"invalid_grant"}`
	_, err = resolveXAIOAuthRuntimeCredentials(env, true, true, 0)
	if !isTerminalXAIOAuthRefreshError(err) {
		t.Fatalf("%v", err)
	}
	state = readAuthStore(t, h)["providers"].(map[string]any)["xai-oauth"].(map[string]any)
	tokens := state["tokens"].(map[string]any)
	marker, _ := state["last_auth_error"].(map[string]any)
	if _, has := tokens["refresh_token"]; has || marker["code"] != "xai_refresh_failed" || marker["reason"] != "runtime_refresh_failure" {
		t.Fatalf("%v", state)
	}
}

func TestXAIDeviceCodeLoginSavesTokensAndSelectsProvider(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	tokenStatus, tokenBody := 200, `{"access_token":"acc","refresh_token":"ref","id_token":"idt"}`
	scriptClient(t, &xaiHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case XAIOAuthDiscoveryURL:
			return textResponse(200, `{"authorization_endpoint":"https://auth.x.ai/oauth2/auth","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
		case XAIOAuthDeviceCodeURL:
			if f := readForm(t, req); f["scope"] != XAIOAuthScope {
				t.Errorf("%v", f)
			}
			return textResponse(200, `{"device_code":"dc","user_code":"UC-1","verification_uri":"https://x.ai/d","verification_uri_complete":"https://x.ai/d?c=UC-1","expires_in":60,"interval":1}`), nil
		}
		if f := readForm(t, req); f["device_code"] != "dc" || f["grant_type"] != DeviceCodeGrantType {
			t.Errorf("%v", f)
		}
		return textResponse(tokenStatus, tokenBody), nil
	})
	var out strings.Builder
	if err := LoginXAIOAuth(env, &out, XAILoginArgs{NoBrowser: true}, true); err != nil {
		t.Fatal(err)
	}
	data := readAuthStore(t, h)
	state := data["providers"].(map[string]any)["xai-oauth"].(map[string]any)
	if data["active_provider"] != "xai-oauth" || state["tokens"].(map[string]any)["access_token"] != "acc" ||
		state["discovery"].(map[string]any)["token_endpoint"] != "https://auth.x.ai/oauth2/token" {
		t.Fatalf("%v", data)
	}
	for _, want := range []string{"Signing in to xAI Grok OAuth (SuperGrok / Premium+)...", "https://x.ai/d?c=UC-1", "UC-1", "polling every 1s", "Login successful!"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %s", want, out.String())
		}
	}
	if raw, err := os.ReadFile(filepath.Join(h, "config.yaml")); err != nil || !strings.Contains(string(raw), "xai-oauth") {
		t.Fatalf("%s %v", raw, err)
	}
	tokenStatus, tokenBody = 400, `{"error":"access_denied","error_description":"declined"}`
	_, err := xaiOAuthDeviceCodeLogin(env, &out, time.Second, false)
	var copyErr *SignInCopyError
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "xai_device_token_failed" || ae.Message != "xAI device-code token polling failed: declined" ||
		!errors.As(err, &copyErr) || copyErr.OAuthErrorCode != "access_denied" {
		t.Fatalf("%v", err)
	}
	tokenStatus, tokenBody = 400, `{"error":"expired_token"}`
	if _, err = xaiOAuthDeviceCodeLogin(env, &out, time.Second, false); !IsDeviceCodeExpired(err) {
		t.Fatalf("%v", err)
	}
}
