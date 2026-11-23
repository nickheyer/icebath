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

// xAI login row a sign in stores, holding the given token pair
func xaiLogin(accessToken, refreshToken string) map[string]any {
	return map[string]any{"id": "xai-login", "label": "xai", "auth_type": "oauth", "priority": 0, "source": SourceManualDeviceCode,
		"access_token": accessToken, "refresh_token": refreshToken, "base_url": DefaultXAIOAuthBaseURL}
}

// Replaces auth.json with one holding the xAI login row
func writeXAILogin(t *testing.T, h string, login map[string]any) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 2, "credential_pool": map[string]any{"xai-oauth": []any{login}}})
}

func TestXAIResolveRefreshesExpiringGrantAndMarksADeadOne(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	writeXAILogin(t, h, xaiLogin(jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 60}), "r0"))
	status, body := 200, `{"access_token":"fresh","refresh_token":"r1"}`
	scriptClient(t, &xaiHTTPClientFn, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == XAIOAuthDiscoveryURL {
			return textResponse(200, `{"authorization_endpoint":"https://auth.x.ai/oauth2/auth","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
		}
		return textResponse(status, body), nil
	})
	creds, err := resolveXAIOAuthRuntimeCredentials(env, false, true, 0)
	if err != nil || creds["api_key"] != "fresh" || creds["source"] != "credential_pool" || creds["base_url"] != DefaultXAIOAuthBaseURL {
		t.Fatalf("%v %v", creds, err)
	}
	if row := poolRows(t, h, "xai-oauth")[0]; row["access_token"] != "fresh" || row["refresh_token"] != "r1" {
		t.Fatalf("%v", row)
	}
	status, body = 400, `{"error":"invalid_grant"}`
	_, err = resolveXAIOAuthRuntimeCredentials(env, true, true, 0)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "xai_refresh_failed" || !ae.ReloginRequired {
		t.Fatalf("%v", err)
	}
	row := poolRows(t, h, "xai-oauth")[0]
	if row["last_status"] != StatusDead || row["last_error_reason"] != "xai_refresh_failed" || row["refresh_token"] != "r1" {
		t.Fatalf("%v", row)
	}
	if _, err = resolveXAIOAuthRuntimeCredentials(env, false, true, 0); !errors.As(err, &ae) || ae.Code != "xai_auth_missing" {
		t.Fatalf("a dead login still resolved: %v", err)
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
	logins := poolRows(t, h, "xai-oauth")
	if len(logins) != 1 || logins[0]["access_token"] != "acc" || logins[0]["refresh_token"] != "ref" || logins[0]["source"] != SourceManualDeviceCode ||
		logins[0]["base_url"] != DefaultXAIOAuthBaseURL {
		t.Fatalf("%v", logins)
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
	_, err = xaiOAuthDeviceCodeLogin(env, &out, time.Second, false)
	var expiredCopy *SignInCopyError
	if !errors.As(err, &expiredCopy) || expiredCopy.OAuthErrorCode != "expired_token" {
		t.Fatalf("%v", err)
	}
}

func TestResolveXAIOAuthRuntimeCredentialsExportedReadsTheLogin(t *testing.T) {
	h := newTestHome(t)
	env := testEnv(t, h)
	token := jwtWithClaims(map[string]any{"exp": time.Now().Unix() + 3600})
	writeXAILogin(t, h, xaiLogin(token, "r0"))
	creds, err := ResolveXAIOAuthRuntimeCredentials(env, false, false, 0)
	if err != nil || creds["api_key"] != token || creds["base_url"] != DefaultXAIOAuthBaseURL {
		t.Fatalf("%v %v", creds, err)
	}
}
