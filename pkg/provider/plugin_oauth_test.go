package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func pluginOAuthTestManifest(tokenURL string) map[string]any {
	return map[string]any{"name": "voice", "requires_env": []any{"VOICE_CLIENT_ID"}, "oauth": []any{map[string]any{
		"id": "acme", "authorize_url": "https://login.acme.example/authorize", "token_url": tokenURL,
		"scopes": []any{"read", "write"}, "client_id_env": "VOICE_CLIENT_ID", "hosts": []any{"api.acme.example"},
	}}}
}

func pluginOAuthTestIdP(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, PoolEnv, OAuthClient) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(handler))
	t.Cleanup(srv.Close)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("VOICE_CLIENT_ID", "client-123")
	env := testEnv(t, h)
	env.HTTP = srv.Client()
	clients, problems := ParseOAuthClients(pluginOAuthTestManifest(srv.URL + "/token"))
	if len(problems) > 0 || len(clients) != 1 {
		t.Fatalf("clients=%v problems=%v", clients, problems)
	}
	return srv, env, clients[0]
}

func TestCustomPrefixNeverRoutesToAPluginGrant(t *testing.T) {
	for _, provider := range []string{"custom:acme", "voice", ":acme", "voice:"} {
		if owned, handled, err := pluginOAuthHandleAuthAction("add", nil, provider); owned || handled || err != nil {
			t.Fatalf("%s: owned=%v handled=%v err=%v", provider, owned, handled, err)
		}
	}
	if _, handled, err := pluginOAuthHandleAuthAction("list", nil, "voice:acme"); handled || err != nil {
		t.Fatalf("unknown action handled=%v err=%v", handled, err)
	}
}

func TestInstallNoticeAndRepinDeltaNameTheOAuthEntry(t *testing.T) {
	manifest := pluginOAuthTestManifest("https://auth.acme.example/token")
	notice := OAuthNotice(manifest)
	if !strings.Contains(notice, "Signs in to acme at login.acme.example, auth.acme.example for api.acme.example with `icebath auth add voice:acme`") {
		t.Fatalf("notice %q", notice)
	}
	if surface := OAuthSurface(manifest); !surface["acme (login.acme.example, auth.acme.example -> api.acme.example; read write)"] || len(surface) != 1 {
		t.Fatalf("surface %v", surface)
	}
	if problems := OAuthAdmissionProblems(map[string]any{"oauth": manifest["oauth"]}, t.TempDir()); len(problems) != 1 ||
		problems[0] != "oauth 'acme': VOICE_CLIENT_ID must be declared under requires_env or optional_env" {
		t.Fatalf("admission %v", problems)
	}
	bad := map[string]any{"oauth": []any{map[string]any{"id": "Bad", "authorize_url": "http://x.example/a", "token_url": "https://u:p@x.example/t",
		"scopes": []any{}, "client_id_env": "lower", "hosts": []any{"10.0.0.1"}, "authorize_params": map[string]any{"a": 1}}}}
	_, problems := ParseOAuthClients(bad)
	want := []string{"oauth[0].id: 'Bad' must be lowercase letters, digits and dashes", "oauth[0].authorize_url: must use https",
		"oauth[0].token_url: must be an absolute URL without user info or fragment", "oauth[0].scopes: must be a non-empty list of scope strings",
		"oauth[0].client_id_env: 'lower' must be an UPPER_SNAKE environment variable name",
		"oauth[0].hosts: must list lowercase DNS host names (no scheme, port, path or IP address)", "oauth[0].authorize_params: must map strings to strings"}
	if strings.Join(problems, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems %q", problems)
	}
	if _, problems := ParseOAuthClients(map[string]any{"oauth": "x"}); len(problems) != 1 || problems[0] != "oauth: must be a list of client entries" {
		t.Fatalf("non list %v", problems)
	}
	if pluginOAuthParent("api.vendor.co.uk") != "vendor.co.uk" || pluginOAuthParent("api.acme.example") != "acme.example" {
		t.Fatal("registered domain")
	}
}

func TestExpiringTokenIsRefreshedOnceAndTheRotationKept(t *testing.T) {
	var calls atomic.Int32
	_, env, client := pluginOAuthTestIdP(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "client-123" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" {
			t.Errorf("form %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-2","expires_in":3600}`))
	})
	key := "voice:acme"
	row := map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_at_ms": 1000, "fingerprint": client.Fingerprint()}
	if err := pluginOAuthWriteGrant(env, key, row); err != nil {
		t.Fatal(err)
	}
	stored, _ := pluginOAuthReadGrant(env, key)
	if token, err := pluginOAuthLiveToken(stored); err != nil || token != "" {
		t.Fatalf("expired token is live: %q %v", token, err)
	}
	token, err := pluginOAuthRefresh(env, client, key, stored)
	if err != nil || token != "at-2" || calls.Load() != 1 {
		t.Fatalf("token=%q err=%v calls=%d", token, err, calls.Load())
	}
	stored, _ = pluginOAuthReadGrant(env, key)
	if stored["access_token"] != "at-2" || stored["refresh_token"] != "rt-1" || stored["fingerprint"] != client.Fingerprint() {
		t.Fatalf("stored %v", stored)
	}
	if live, err := pluginOAuthLiveToken(stored); err != nil || live != "at-2" {
		t.Fatalf("rotated token not live: %q %v", live, err)
	}
}

func TestRejectedClientIsAnAuthErrorThatLeavesTheGrant(t *testing.T) {
	_, env, client := pluginOAuthTestIdP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"secret-ish detail"}`))
	})
	key := "voice:acme"
	row := map[string]any{"access_token": "at-1", "refresh_token": "rt-1", "expires_at_ms": 1000, "fingerprint": client.Fingerprint()}
	if err := pluginOAuthWriteGrant(env, key, row); err != nil {
		t.Fatal(err)
	}
	_, err := pluginOAuthRefresh(env, client, key, row)
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "invalid_client" ||
		authErr.Message != "voice:acme: the token endpoint refused the request (HTTP 401, invalid_client)." {
		t.Fatalf("err %#v", err)
	}
	if stored, _ := pluginOAuthReadGrant(env, key); stored["refresh_token"] != "rt-1" || stored["dead"] != nil {
		t.Fatalf("grant changed %v", stored)
	}
}

func TestPartialConsentSavesNothingAndNamesWhatIsMissing(t *testing.T) {
	_, env, client := pluginOAuthTestIdP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","scope":"read"}`))
	})
	_, err := pluginOAuthTokenRequest(env, client, "voice:acme")(map[string]string{"grant_type": "authorization_code", "code": "c"})
	var authErr *AuthError
	if !errors.As(err, &authErr) || authErr.Code != "oauth_scope_not_granted" || !strings.HasSuffix(authErr.Message, "Missing: write") {
		t.Fatalf("err %v", err)
	}
	t.Setenv("VOICE_CLIENT_ID", "")
	_, err = pluginOAuthTokenRequest(env, client, "voice:acme")(map[string]string{"grant_type": "refresh_token"})
	if !errors.As(err, &authErr) || authErr.Code != "oauth_client_id_missing" {
		t.Fatalf("missing client env err %v", err)
	}
}

func TestOtherAuthStoreWritesKeepThePluginGrant(t *testing.T) {
	h := t.TempDir()
	env := testEnv(t, h)
	if err := pluginOAuthWriteGrant(env, "voice:acme", map[string]any{"access_token": "at"}); err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Update(func(data AuthData) (bool, error) { data["active_provider"] = "nous"; return true, nil }); err != nil {
		t.Fatal(err)
	}
	if row, _ := pluginOAuthReadGrant(env, "voice:acme"); row["access_token"] != "at" {
		t.Fatalf("grant lost %v", row)
	}
	if err := pluginOAuthWriteGrant(env, "voice:acme", nil); err != nil {
		t.Fatal(err)
	}
	if row, _ := pluginOAuthReadGrant(env, "voice:acme"); row != nil {
		t.Fatalf("grant kept after removal %v", row)
	}
}
