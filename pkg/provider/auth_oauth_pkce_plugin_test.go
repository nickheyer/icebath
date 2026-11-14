package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- tests/providers/test_oauth_pkce_plugin.py and fake_pkce_idp.py ----

const pkceTestProvider = "example-pkce"

// Local OAuth IdP that 302s authorize straight to the loopback callback and rotates single use refresh tokens
type fakePKCEIdP struct {
	mu            sync.Mutex
	codes         map[string]string
	refreshTokens map[string]bool
	tokenRequests []map[string]string
	issued        int
	overrideState string
	browserStatus []int
	base          string
}

func newFakePKCEIdP(t *testing.T) *fakePKCEIdP {
	t.Helper()
	idp := &fakePKCEIdP{codes: map[string]string{}, refreshTokens: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(idp.serve))
	t.Cleanup(server.Close)
	idp.base = server.URL
	return idp
}

func (idp *fakePKCEIdP) serve(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	reply := func(status int, payload map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		raw, _ := jsonMarshal(payload)
		w.Write(raw)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/authorize":
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" {
			reply(400, map[string]any{"error": "invalid_request"})
			return
		}
		code := pkceTokenURLSafe(16)
		idp.codes[code] = q.Get("code_challenge")
		state := q.Get("state")
		if idp.overrideState != "" {
			state = idp.overrideState
		}
		w.Header().Set("Location", q.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {state}}.Encode())
		w.WriteHeader(http.StatusFound)
	case r.Method == http.MethodPost && r.URL.Path == "/token":
		r.ParseForm()
		form := map[string]string{}
		for k, v := range r.PostForm {
			form[k] = v[0]
		}
		idp.tokenRequests = append(idp.tokenRequests, form)
		switch form["grant_type"] {
		case "authorization_code":
			challenge, ok := idp.codes[form["code"]]
			delete(idp.codes, form["code"])
			sum := sha256.Sum256([]byte(form["code_verifier"]))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
				reply(400, map[string]any{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if !idp.refreshTokens[form["refresh_token"]] {
				reply(400, map[string]any{"error": "invalid_grant"})
				return
			}
			delete(idp.refreshTokens, form["refresh_token"])
		default:
			reply(400, map[string]any{"error": "unsupported_grant_type"})
			return
		}
		idp.issued++
		refresh := fmt.Sprintf("rt-%d-%s", idp.issued, pkceTokenURLSafe(4))
		idp.refreshTokens[refresh] = true
		reply(200, map[string]any{"access_token": fmt.Sprintf("at-%d-%s", idp.issued, pkceTokenURLSafe(4)), "refresh_token": refresh, "expires_in": 3600, "token_type": "Bearer"})
	default:
		w.WriteHeader(404)
	}
}

func jsonMarshal(v any) ([]byte, error) { return marshalIndented(v) }

func (idp *fakePKCEIdP) requests() []map[string]string {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return append([]map[string]string{}, idp.tokenRequests...)
}

func (idp *fakePKCEIdP) holds(refresh string) bool {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	return idp.refreshTokens[refresh]
}

func (idp *fakePKCEIdP) config() OAuthPKCEConfig {
	cfg := NewOAuthPKCEConfig("hermes-example", idp.base+"/authorize", idp.base+"/token")
	cfg.Scopes = []string{"inference"}
	cfg.TimeoutSeconds = 5
	return cfg
}

// Browser double that follows the authorize redirect to the loopback callback the way a real browser would
func (idp *fakePKCEIdP) installBrowser(t *testing.T) {
	scriptBrowserEnv(t, map[string]string{"BROWSER": "firefox", "DISPLAY": ":0"}, "firefox")
	prev := openBrowserFn
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	openBrowserFn = func(target string) error {
		go func() {
			resp, err := client.Get(target)
			status := 0
			if err == nil {
				status = resp.StatusCode
				resp.Body.Close()
			}
			idp.mu.Lock()
			idp.browserStatus = append(idp.browserStatus, status)
			idp.mu.Unlock()
		}()
		return nil
	}
	t.Cleanup(func() { openBrowserFn = prev })
}

func capturePKCEOut(t *testing.T) *bytes.Buffer {
	buf := &bytes.Buffer{}
	prev := pkceOut
	pkceOut = buf
	t.Cleanup(func() { pkceOut = prev })
	return buf
}

func authErrCode(err error) string {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestPKCEHandlerAddStatusRefreshLogoutAgainstFakeIdP(t *testing.T) {
	h := newTestHome(t)
	idp := newFakePKCEIdP(t)
	idp.installBrowser(t)
	out := capturePKCEOut(t)
	hooks := PKCEProviderProfileHooks(idp.config())
	registerHookedProfile(t, pkceTestProvider, hooks)

	if ok, err := hooks.AuthHandler("add", nil, pkceTestProvider); !ok || err != nil {
		t.Fatalf("add %v %v", ok, err)
	}
	reqs := idp.requests()
	exchange := reqs[len(reqs)-1]
	if exchange["grant_type"] != "authorization_code" || exchange["code_verifier"] == "" || !strings.HasPrefix(exchange["redirect_uri"], "http://127.0.0.1:") {
		t.Fatalf("exchange %v", exchange)
	}
	rows := poolRows(t, h, pkceTestProvider)
	if len(rows) != 1 || rows[0]["auth_type"] != "oauth" || rows[0]["source"] != PKCEPoolSource || rows[0]["oauth_pkce"].(map[string]any)["client_id"] != "hermes-example" {
		t.Fatalf("rows %v", rows)
	}
	if !idp.holds(anyString(rows[0]["refresh_token"])) || !truthyAny(rows[0]["expires_at_ms"]) {
		t.Fatalf("row tokens %v", rows[0])
	}
	if !strings.Contains(out.String(), "added to the pool") {
		t.Fatalf("add output %q", out.String())
	}
	if ok, err := hooks.AuthHandler("status", nil, pkceTestProvider); !ok || err != nil || !strings.Contains(out.String(), pkceTestProvider+": logged in") {
		t.Fatalf("status %v %v %q", ok, err, out.String())
	}
	if ok, _ := hooks.AuthHandler("refresh", nil, pkceTestProvider); ok {
		t.Fatal("refresh must be declined to the pool")
	}

	before := anyString(rows[0]["refresh_token"])
	pool := LoadPool(testEnv(t, h), pkceTestProvider)
	rotated, ok := pool.TryRefreshMatching("", anyString(rows[0]["id"]))
	if !ok || rotated.RefreshToken == before || rotated.AccessToken == rows[0]["access_token"] {
		t.Fatalf("rotated %+v", rotated)
	}
	reqs = idp.requests()
	want := map[string]string{"grant_type": "refresh_token", "refresh_token": before, "scope": "inference", "client_id": "hermes-example"}
	if !reflect.DeepEqual(reqs[len(reqs)-1], want) || idp.holds(before) {
		t.Fatalf("refresh request %v", reqs[len(reqs)-1])
	}
	stale := LoadPool(testEnv(t, h), pkceTestProvider).Entries()[0]
	stale.RefreshToken = before
	adopted, err := hooks.RefreshCredential(stale)
	if err != nil || adopted["refresh_token"] != rotated.RefreshToken {
		t.Fatalf("peer adoption %v %v", adopted, err)
	}
	if got := idp.requests(); len(got) != len(reqs) {
		t.Fatal("peer adoption spent the refresh token again")
	}

	if ok, err := hooks.AuthHandler("logout", nil, pkceTestProvider); !ok || err != nil {
		t.Fatalf("logout %v %v", ok, err)
	}
	if n := len(LoadPool(testEnv(t, h), pkceTestProvider).Entries()); n != 0 {
		t.Fatalf("%d entries after logout", n)
	}

	idp.mu.Lock()
	idp.overrideState = "forged"
	idp.mu.Unlock()
	posts := len(idp.requests())
	if _, err := hooks.AuthHandler("add", nil, pkceTestProvider); authErrCode(err) != "oauth_state_mismatch" {
		t.Fatalf("forged state %v", err)
	}
	if len(idp.requests()) != posts {
		t.Fatal("a forged callback reached the token endpoint")
	}
	idp.mu.Lock()
	defer idp.mu.Unlock()
	for _, status := range idp.browserStatus {
		if status != 200 {
			t.Fatalf("browser hits %v", idp.browserStatus)
		}
	}
}

// Transport failing the test on any request
type refuseTransport struct{ t *testing.T }

func (rt refuseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.t.Errorf("token endpoint must not be contacted: %s", req.URL)
	return nil, errors.New("refused")
}

func TestPKCETokenURLOffAuthorizeAllowlistRefusedBeforeAnyRequest(t *testing.T) {
	newTestHome(t)
	idp := newFakePKCEIdP(t)
	capturePKCEOut(t)
	prev := pkceEnvFn
	pkceEnvFn = func() PoolEnv {
		env := prev()
		env.HTTP = &http.Client{Transport: refuseTransport{t}}
		return env
	}
	t.Cleanup(func() { pkceEnvFn = prev })
	cfg := idp.config()
	cfg.TokenURL = "https://token.attacker.example/token"
	_, loginErr := LoginOAuthPKCE(pkceTestProvider, cfg, false)
	_, refreshErr := PKCERefreshCredential(cfg)(PooledCredential{Provider: pkceTestProvider, ID: "abc123", RefreshToken: "rt", AccessToken: "at"})
	if authErrCode(loginErr) != "oauth_token_host_rejected" || authErrCode(refreshErr) != "oauth_token_host_rejected" {
		t.Fatalf("login %v refresh %v", loginErr, refreshErr)
	}
	bad := idp.config()
	bad.AuthorizeURL = "http://auth.example.com/authorize"
	if err := ValidateOAuthPKCEConfig(pkceTestProvider, bad); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain http authorize %v", err)
	}
}

func TestPKCESpentRefreshTokenIsGrantDeadAndMarksThePoolRowDead(t *testing.T) {
	h := newTestHome(t)
	idp := newFakePKCEIdP(t)
	idp.installBrowser(t)
	capturePKCEOut(t)
	hooks := PKCEProviderProfileHooks(idp.config())
	registerHookedProfile(t, pkceTestProvider, hooks)
	if ok, err := hooks.AuthHandler("add", nil, pkceTestProvider); !ok || err != nil {
		t.Fatalf("add %v %v", ok, err)
	}
	rows := poolRows(t, h, pkceTestProvider)
	spent := anyString(rows[0]["refresh_token"])
	idp.mu.Lock()
	delete(idp.refreshTokens, spent)
	idp.mu.Unlock()

	_, err := hooks.RefreshCredential(PooledCredential{Provider: pkceTestProvider, ID: anyString(rows[0]["id"]), RefreshToken: spent, AccessToken: anyString(rows[0]["access_token"])})
	if authErrCode(err) != "invalid_grant" {
		t.Fatalf("spent refresh %v", err)
	}
	pool := LoadPool(testEnv(t, h), pkceTestProvider)
	if result, ok := pool.TryRefreshMatching("", anyString(rows[0]["id"])); ok && result.LastStatus != StatusDead {
		t.Fatalf("result %+v", result)
	}
	if got := poolRows(t, h, pkceTestProvider)[0]["last_status"]; got != StatusDead {
		t.Fatalf("disk status %v", got)
	}
}

func TestPKCEAliasLoginStoresTheRowUnderTheCanonicalProfileName(t *testing.T) {
	h := newTestHome(t)
	idp := newFakePKCEIdP(t)
	idp.installBrowser(t)
	out := capturePKCEOut(t)
	alias, canonical := "x-ai", "xai"
	if NormalizeProvider(alias) != canonical {
		t.Fatalf("%s is not an alias of %s", alias, canonical)
	}
	handler := PKCEAuthHandler(idp.config())
	if ok, err := handler("add", nil, alias); !ok || err != nil {
		t.Fatalf("add %v %v", ok, err)
	}
	pool := readAuthStore(t, h)["credential_pool"].(map[string]any)
	if _, ok := pool[canonical]; !ok {
		t.Fatalf("pool %v", pool)
	}
	if _, ok := pool[alias]; ok {
		t.Fatal("row stored under the alias")
	}
	if ok, err := handler("status", nil, alias); !ok || err != nil || !strings.Contains(out.String(), canonical+": logged in") {
		t.Fatalf("status %v %v %q", ok, err, out.String())
	}
	if got := LoadPool(testEnv(t, h), canonical).Entries()[0].Provider; got != canonical {
		t.Fatalf("entry provider %s", got)
	}
}

// ---- tests/plugins/model_providers/test_solstice_profile.py brokered PKCE refresh ----

func TestPKCEBrokeredRefreshThroughThePool(t *testing.T) {
	h := newTestHome(t)
	name := "example-brokered"
	var grants []map[string]string
	broker := func(grant map[string]string) (map[string]any, error) {
		grants = append(grants, grant)
		return map[string]any{"access_token": fmt.Sprintf("ya29.fresh-%d", len(grants)), "expires_in": float64(3600)}, nil
	}
	cfg := NewOAuthPKCEConfig("", "", "")
	cfg.TokenRequest = broker
	registerHookedProfile(t, name, PKCEProviderProfileHooks(cfg))
	writeRow := func(access string, expiresAtMS int64) {
		writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{name: []any{map[string]any{
			"id": "abc123", "label": name, "auth_type": "oauth", "priority": 0, "source": PKCEPoolSource,
			"access_token": access, "refresh_token": "1//rt-0", "expires_at_ms": expiresAtMS}}}})
	}
	nowMS := time.Now().UnixMilli()
	writeRow("ya29.live", nowMS+3_600_000)
	if sel, ok := LoadPool(testEnv(t, h), name).Select(""); !ok || sel.AccessToken != "ya29.live" || len(grants) != 0 {
		t.Fatalf("fresh row %+v grants %v", sel, grants)
	}
	writeRow("ya29.stale", nowMS-1000)
	if sel, ok := LoadPool(testEnv(t, h), name).Select(""); !ok || sel.AccessToken != "ya29.fresh-1" {
		t.Fatalf("stale row %+v", sel)
	}
	if !reflect.DeepEqual(grants, []map[string]string{{"grant_type": "refresh_token", "refresh_token": "1//rt-0"}}) {
		t.Fatalf("grants %v", grants)
	}
	disk := poolRows(t, h, name)[0]
	if disk["access_token"] != "ya29.fresh-1" || disk["refresh_token"] != "1//rt-0" {
		t.Fatalf("disk %v", disk)
	}
	if exp, _ := anyFloat(disk["expires_at_ms"]); int64(exp) <= nowMS+3_000_000 {
		t.Fatalf("expiry %v", disk["expires_at_ms"])
	}

	deadCfg := NewOAuthPKCEConfig("", "", "")
	deadCfg.TokenRequest = func(map[string]string) (map[string]any, error) {
		return nil, &AuthError{Message: "rejected", Provider: name, Code: "invalid_grant", ReloginRequired: true}
	}
	RegisterProvider(Profile{Name: name, AuthType: AuthTypeOAuthExternal, BaseURL: "https://example.invalid/v1", RefreshCredential: PKCERefreshCredential(deadCfg)})
	writeRow("ya29.revoked", nowMS+3_600_000)
	pool := LoadPool(testEnv(t, h), name)
	pool.Select("")
	if _, ok := pool.TryRefreshCurrent(); ok {
		t.Fatal("a rejected grant refreshed")
	}
	pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, APIKeyHint: "ya29.revoked"})
	if got := poolRows(t, h, name)[0]["last_status"]; got != StatusDead {
		t.Fatalf("disk status %v", got)
	}
}

func TestPKCEConfigValidationAndTokenFields(t *testing.T) {
	cfg := NewOAuthPKCEConfig("client", "https://auth.example.com/authorize", "https://login.example.com/token")
	if err := ValidateOAuthPKCEConfig("p", cfg); authErrCode(err) != "oauth_token_host_rejected" || !strings.Contains(err.Error(), "'login.example.com' is not on the allowlist ['auth.example.com']") {
		t.Fatalf("allowlist %v", err)
	}
	cfg.AllowedHosts = []string{"Example.com"}
	if err := ValidateOAuthPKCEConfig("p", cfg); err != nil {
		t.Fatalf("subdomain of an allowed apex %v", err)
	}
	cfg.RedirectPort = 70000
	if err := ValidateOAuthPKCEConfig("p", cfg); authErrCode(err) != "oauth_redirect_invalid" {
		t.Fatalf("port %v", err)
	}
	if err := ValidateOAuthPKCEConfig("p", NewOAuthPKCEConfig(" ", "https://a", "https://a")); authErrCode(err) != "oauth_client_id_missing" {
		t.Fatalf("client id %v", err)
	}
	if err := ValidateOAuthPKCEConfig("p", NewOAuthPKCEConfig("c", "http://127.0.0.1:9/a", "http://127.0.0.1:9/t")); err != nil {
		t.Fatalf("loopback http %v", err)
	}
	if err := ValidateOAuthPKCEConfig("p", NewOAuthPKCEConfig("c", "https:///a", "https://a/t")); authErrCode(err) != "oauth_endpoint_invalid" {
		t.Fatalf("hostless %v", err)
	}
	fields, err := pkceTokenFields("p", map[string]any{"access_token": " at "}, map[string]string{"refresh_token": "old"}, "c")
	if err != nil || fields["access_token"] != "at" || fields["refresh_token"] != "old" || fields["expires_at_ms"] != nil {
		t.Fatalf("fields %v %v", fields, err)
	}
	if _, err := pkceTokenFields("p", map[string]any{}, nil, "oauth_refresh_failed"); authErrCode(err) != "oauth_refresh_failed" {
		t.Fatalf("missing access token %v", err)
	}
	for body, wantCode := range map[string]string{`{"error":"invalid_grant"}`: "invalid_grant", `{"error":"server_error"}`: "fallback", `not json`: "fallback"} {
		got := pkceTokenHTTPError("p", &oauthHTTPResponse{Status: 400, Body: []byte(body)}, "fallback")
		if got.Code != wantCode {
			t.Fatalf("%s mapped to %s", body, got.Code)
		}
	}
	if !pkceIsUsable("at", nil, 5) || pkceIsUsable(" ", nil, 5) || pkceIsUsable("at", float64(5), 5) || !pkceIsUsable("at", float64(6), 5) {
		t.Fatal("usable")
	}
}
