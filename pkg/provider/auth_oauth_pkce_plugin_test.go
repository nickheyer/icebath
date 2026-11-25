package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	cfg := NewOAuthPKCEConfig("icebath-example", idp.base+"/authorize", idp.base+"/token")
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

func authErrCode(err error) string {
	var ae *AuthError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestPKCELoginExchangesTheCodeAndRefusesAForgedStateAgainstFakeIdP(t *testing.T) {
	newTestHome(t)
	idp := newFakePKCEIdP(t)
	idp.installBrowser(t)
	out := &bytes.Buffer{}

	tokens, err := pkceLogin(pkceEnvFn(), out, pkceTestProvider, idp.config(), true)
	if err != nil {
		t.Fatalf("login %v", err)
	}
	reqs := idp.requests()
	exchange := reqs[len(reqs)-1]
	if exchange["grant_type"] != "authorization_code" || exchange["code_verifier"] == "" || !strings.HasPrefix(exchange["redirect_uri"], "http://127.0.0.1:") {
		t.Fatalf("exchange %v", exchange)
	}
	if !idp.holds(anyString(tokens["refresh_token"])) || tokens["expires_at_ms"] == nil {
		t.Fatalf("tokens %v", tokens)
	}
	if !strings.Contains(out.String(), "Open this URL to authorize Icebath with "+pkceTestProvider) {
		t.Fatalf("login output %q", out.String())
	}

	idp.mu.Lock()
	idp.overrideState = "forged"
	idp.mu.Unlock()
	posts := len(idp.requests())
	if _, err := pkceLogin(pkceEnvFn(), out, pkceTestProvider, idp.config(), true); authErrCode(err) != "oauth_state_mismatch" {
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
	prev := pkceEnvFn
	pkceEnvFn = func() PoolEnv {
		env := prev()
		env.HTTP = &http.Client{Transport: refuseTransport{t}}
		return env
	}
	t.Cleanup(func() { pkceEnvFn = prev })
	cfg := idp.config()
	cfg.TokenURL = "https://token.attacker.example/token"
	if _, loginErr := pkceLogin(pkceEnvFn(), io.Discard, pkceTestProvider, cfg, false); authErrCode(loginErr) != "oauth_token_host_rejected" {
		t.Fatalf("login %v", loginErr)
	}
	bad := idp.config()
	bad.AuthorizeURL = "http://auth.example.com/authorize"
	if err := ValidateOAuthPKCEConfig(pkceTestProvider, bad); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("plain http authorize %v", err)
	}
}

func TestPKCEConfigValidationAndTokenFields(t *testing.T) {
	cfg := NewOAuthPKCEConfig("client", "https://auth.example.com/authorize", "https://login.example.com/token")
	if err := ValidateOAuthPKCEConfig("p", cfg); authErrCode(err) != "oauth_token_host_rejected" || !strings.Contains(err.Error(), `"login.example.com" is not on the allowlist ["auth.example.com"]`) {
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
}
