package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSolsticeIsOffEveryDiscoverySurfaceUntilSignedInButAlwaysResolves(t *testing.T) {
	h := newTestHome(t)
	ctx := context.Background()
	surfaces := func() map[string]bool {
		out := map[string]bool{}
		for _, p := range listedCanonicalProviders() {
			out["canonical"] = out["canonical"] || p.Slug == "solstice"
		}
		for _, r := range ListAvailableProviders(ctx) {
			out["provider_list"] = out["provider_list"] || r.ID == "solstice"
		}
		for _, d := range ProviderCatalog() {
			out["setup_catalog"] = out["setup_catalog"] || d.Slug == "solstice"
		}
		return out
	}
	if !bundledPluginProfile(t, "solstice").Hidden {
		t.Fatal("solstice must be hidden")
	}
	t.Setenv("ICEBATH_HOME", h)
	if s := surfaces(); s["canonical"] || s["provider_list"] || s["setup_catalog"] {
		t.Fatalf("signed out surfaces %v", s)
	}
	env := testEnv(t, h)
	for _, name := range []string{"solstice", "solstice-oauth"} {
		if got, err := resolveProvider(env, name); err != nil || got != "solstice" {
			t.Fatalf("resolveProvider(%s) = %q, %v", name, got, err)
		}
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"solstice": []any{map[string]any{
		"id": "abc123", "label": "solstice", "auth_type": "oauth", "priority": 0, "source": PKCEPoolSource,
		"access_token": "ya29.x", "refresh_token": "1//rt-0", "expires_at_ms": time.Now().UnixMilli() + 3_600_000}}}})
	if s := surfaces(); !s["canonical"] || !s["provider_list"] || !s["setup_catalog"] {
		t.Fatalf("signed in surfaces %v", s)
	}
}

// Fake Nous portal broker recording each request and answering per path
func solsticeFakeNAS(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body map[string]any)) (*[]string, string) {
	t.Helper()
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": fullNousStateFixture()}})
	token := anyString(fullNousStateFixture()["access_token"])
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("request without the Nous bearer %q", r.Header.Get("Authorization"))
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		seen = append(seen, r.Method+" "+r.URL.Path)
		handler(w, r, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ICEBATH_PORTAL_BASE_URL", srv.URL)
	env := testEnv(t, h)
	env.HTTP = srv.Client()
	orig := pkceEnvFn
	t.Cleanup(func() { pkceEnvFn = orig })
	pkceEnvFn = func() PoolEnv { return env }
	return &seen, token
}

func TestSolsticeBrokerDiscoversClientAndBrokersGrants(t *testing.T) {
	var refreshStatus int
	var lastBody map[string]any
	seen, _ := solsticeFakeNAS(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		lastBody = body
		switch r.URL.Path {
		case solsticeConfigPath:
			io.WriteString(w, `{"client_id":" cid ","authorize_url":"https://accounts.google.com/o/oauth2/v2/auth","scope":"openid email"}`)
		case "/api/oauth/gemini-auth/exchange":
			io.WriteString(w, `{"access_token":"ya29.new","refresh_token":"1//rt","expires_in":3600}`)
		case "/api/oauth/gemini-auth/refresh":
			w.WriteHeader(refreshStatus)
			io.WriteString(w, `{"access_token":"ya29.fresh","expires_in":3600}`)
		}
	})
	client, err := solsticeDiscoverClient()
	if err != nil || !reflect.DeepEqual(client, map[string]string{"client_id": "cid", "authorize_url": "https://accounts.google.com/o/oauth2/v2/auth", "scope": "openid email"}) {
		t.Fatalf("discover %v %v", client, err)
	}
	payload, err := solsticeBrokerTokenRequest(map[string]string{"grant_type": "authorization_code", "code": "c", "code_verifier": "v",
		"redirect_uri": "http://127.0.0.1:1/gemini-auth/callback", "client_id": "dropped"})
	if err != nil || payload["access_token"] != "ya29.new" {
		t.Fatalf("exchange %v %v", payload, err)
	}
	if !reflect.DeepEqual(lastBody, map[string]any{"code": "c", "code_verifier": "v", "redirect_uri": "http://127.0.0.1:1/gemini-auth/callback"}) {
		t.Fatalf("exchange body %v", lastBody)
	}
	refreshStatus = 200
	if payload, err := solsticeBrokerTokenRequest(map[string]string{"grant_type": "refresh_token", "refresh_token": "1//rt"}); err != nil || payload["access_token"] != "ya29.fresh" {
		t.Fatalf("refresh %v %v", payload, err)
	}
	refreshStatus = 502
	_, err = solsticeBrokerTokenRequest(map[string]string{"grant_type": "refresh_token", "refresh_token": "1//rt"})
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "invalid_grant" || !ae.ReloginRequired || ae.Message != "solstice: the stored sign-in was rejected; run `icebath auth add solstice` again." {
		t.Fatalf("rejected refresh %v", err)
	}
	want := []string{"GET " + solsticeConfigPath, "POST /api/oauth/gemini-auth/exchange", "POST /api/oauth/gemini-auth/refresh", "POST /api/oauth/gemini-auth/refresh"}
	if !reflect.DeepEqual(*seen, want) {
		t.Fatalf("requests %v", *seen)
	}
}

func TestSolsticeBrokerErrorCodes(t *testing.T) {
	status, body := 404, `{}`
	solsticeFakeNAS(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	code := func() string {
		_, err := solsticeDiscoverClient()
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("not an auth error %v", err)
		}
		return ae.Code + "|" + ae.Message
	}
	if got := code(); got != "solstice_not_configured|solstice: not available on this Nous account yet." {
		t.Fatal(got)
	}
	status = 500
	if got := code(); got != "solstice_broker_failed|solstice: Nous portal returned HTTP 500." {
		t.Fatal(got)
	}
	status, body = 200, `{"client_id":"cid","authorize_url":"","scope":0}`
	if got := code(); got != "solstice_config_invalid|solstice: Nous portal config is missing authorize_url, scope." {
		t.Fatal(got)
	}
	if _, err := solsticeBrokerTokenRequest(map[string]string{"grant_type": "refresh_token", "refresh_token": "x"}); err != nil {
		t.Fatalf("a 200 refresh must pass through %v", err)
	}
}

func TestSolsticeBrokerNeedsNousLogin(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	env := testEnv(t, h)
	orig := pkceEnvFn
	t.Cleanup(func() { pkceEnvFn = orig })
	pkceEnvFn = func() PoolEnv { return env }
	_, err := solsticeDiscoverClient()
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != "solstice_nous_login_required" || !ae.ReloginRequired {
		t.Fatalf("missing Nous login %v", err)
	}
}

func TestSolsticeClientSpeaksPerUserQuotaMethodsWithBearer(t *testing.T) {
	p := bundledPluginProfile(t, "solstice")
	if _, err := p.CreateClient(context.Background(), map[string]any{"api_key": " "}); err == nil || err.Error() != solsticeMissingKeyError {
		t.Fatalf("blank key %v", err)
	}
	c, err := p.CreateClient(context.Background(), map[string]any{"api_key": "ya29.tok", "default_headers": map[string]string{"X-Extra": "1"}, "ignored": true})
	if err != nil {
		t.Fatal(err)
	}
	gc := c.(*geminiNativeClient)
	if gc.generateMethod != "generateContentPerUserQuota" || gc.streamMethod != "streamGenerateContentPerUserQuota" {
		t.Fatalf("methods %s %s", gc.generateMethod, gc.streamMethod)
	}
	base, auth, err := gc.credentials(context.Background())
	if err != nil || auth["Authorization"] != "Bearer ya29.tok" || auth["x-goog-api-key"] != "" {
		t.Fatalf("credentials %v %v", auth, err)
	}
	if base != normalizeGeminiBaseURL(solsticeInferenceBaseURL) || gc.headers(auth, "").Get("X-Extra") != "1" {
		t.Fatalf("base %s", base)
	}
	if !gc.bearer {
		t.Fatal("bearer clients skip the API key guidance")
	}
}
