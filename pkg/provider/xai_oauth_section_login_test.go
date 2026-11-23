package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestXAIOAuthSectionLoginSavesTokensWithoutSwitchingProvider(t *testing.T) {
	h, env := authTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 2, "providers": map[string]any{}})
	prevLogin, prevGetenv := xaiOAuthDeviceCodeLoginFn, oauthGetenv
	t.Cleanup(func() { xaiOAuthDeviceCodeLoginFn, oauthGetenv = prevLogin, prevGetenv })
	oauthGetenv = func(string) string { return "" }
	xaiOAuthDeviceCodeLoginFn = func(_ PoolEnv, out io.Writer, _ time.Duration, _ bool) (map[string]any, error) {
		io.WriteString(out, "visit the device page\n")
		return map[string]any{
			"tokens":    map[string]any{"access_token": "section-access", "refresh_token": "section-refresh", "id_token": "", "token_type": "Bearer"},
			"discovery": map[string]any{"token_endpoint": "https://auth.x.ai/oauth2/token"}, "redirect_uri": "", "last_refresh": "2026-07-25T12:00:00Z",
		}, nil
	}
	if err := env.Store.SuppressCredentialSource("xai-oauth", "device_code"); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if !XAIOAuthSectionLogin(env, &out) || env.Store.IsSourceSuppressed("xai-oauth", "device_code") {
		t.Fatalf("login failed or left the source suppressed, output %q", out.String())
	}
	if out.String() != "Signing in to xAI Grok OAuth (SuperGrok / Premium+)...\nvisit the device page\n" {
		t.Fatalf("output %q", out.String())
	}
	if logins := poolRows(t, h, "xai-oauth"); len(logins) != 1 || logins[0]["access_token"] != "section-access" || logins[0]["refresh_token"] != "section-refresh" {
		t.Fatalf("xai logins %v", logins)
	}
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) {
		return nil, errors.New("authorization_pending timed out")
	}
	out.Reset()
	if XAIOAuthSectionLogin(env, &out) || !strings.Contains(out.String(), "xAI Grok OAuth login failed: authorization_pending timed out") {
		t.Fatalf("output %q", out.String())
	}
	xaiOAuthDeviceCodeLoginFn = func(PoolEnv, io.Writer, time.Duration, bool) (map[string]any, error) { return map[string]any{}, nil }
	out.Reset()
	if XAIOAuthSectionLogin(env, &out) || !strings.Contains(out.String(), "login failed") {
		t.Fatalf("a result without tokens passed, output %q", out.String())
	}
}
