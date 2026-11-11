package dashboardauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const (
	oidcIssuer   = "https://auth.example.com/application/o/hermes"
	oidcClientID = "hermes-dashboard"
	oidcKID      = "test-key-1"
)

var (
	oidcKeyOnce sync.Once
	oidcKey     *rsa.PrivateKey
)

func oidcFixtureKey() *rsa.PrivateKey {
	oidcKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		oidcKey = k
	})
	return oidcKey
}

func b64urlUint(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }

// RS256 token over claims with the fixture kid header, signed by key
func signRS256(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","kid":"`+oidcKID+`","typ":"JWT"}`)) + "." + enc.EncodeToString(body)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

// ID token of the fixture key with the hermes case defaults, overrides applied last
func mintIDToken(t *testing.T, ttlSeconds int64, overrides map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{"iss": oidcIssuer, "aud": oidcClientID, "sub": "usr_abc", "iat": now, "exp": now + ttlSeconds,
		"email": "alice@example.com", "name": "Alice Example"}
	for k, v := range overrides {
		claims[k] = v
	}
	return signRS256(t, oidcFixtureKey(), claims)
}

// Loopback JWKS endpoint serving the fixture public key
func oidcJWKSURL(t *testing.T) string {
	t.Helper()
	pub := oidcFixtureKey().PublicKey
	doc, _ := json.Marshal(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": oidcKID,
		"n": b64urlUint(pub.N), "e": b64urlUint(big.NewInt(int64(pub.E)))}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(doc)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/jwks"
}

// Provider with the hermes discovery document seeded and its JWKS client bound to the fixture key
func makeOIDCProvider(t *testing.T, scopes, clientSecret string, authMethods []string) *SelfHostedOIDCProvider {
	t.Helper()
	p, err := NewSelfHostedOIDCProvider(oidcIssuer, oidcClientID, scopes, clientSecret, DefaultTokenLeewaySeconds)
	if err != nil {
		t.Fatal(err)
	}
	if authMethods == nil {
		authMethods = []string{}
	}
	p.discovery = &oidcDiscovery{issuer: oidcIssuer, authorizationEndpoint: oidcIssuer + "/authorize",
		tokenEndpoint: oidcIssuer + "/token", jwksURI: oidcIssuer + "/jwks", revocationEndpoint: oidcIssuer + "/revoke",
		authMethods: authMethods}
	p.discoveryFetchedAt = float64(time.Now().UnixNano()) / 1e9
	p.setJWKSClient(makeJWKSClient(oidcJWKSURL(t)))
	return p
}

// Canned transport recording every request it answers
type cannedTransport struct {
	mu       sync.Mutex
	status   int
	body     string
	ctype    string
	finalURL string
	fail     error
	calls    []*http.Request
	forms    []url.Values
}

func (c *cannedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	form := url.Values{}
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		form, _ = url.ParseQuery(string(raw))
	}
	c.calls = append(c.calls, req)
	c.forms = append(c.forms, form)
	if c.fail != nil {
		return nil, c.fail
	}
	answered := req
	if c.finalURL != "" {
		u, _ := url.Parse(c.finalURL)
		answered = req.Clone(req.Context())
		answered.URL = u
	}
	return &http.Response{StatusCode: c.status, Header: http.Header{"Content-Type": {c.ctype}},
		Body: io.NopCloser(bytes.NewBufferString(c.body)), Request: answered}, nil
}

func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }

// Answers every token endpoint POST with the canned response until cleanup
func mockTokenPost(t *testing.T, status int, body string) *cannedTransport {
	t.Helper()
	rt := &cannedTransport{status: status, body: body, ctype: "application/json"}
	prev := tokenHTTP
	tokenHTTP = &http.Client{Transport: rt, CheckRedirect: pyhttpx.RedirectPolicy(false)}
	t.Cleanup(func() { tokenHTTP = prev })
	return rt
}

// Answers every discovery GET with the canned document served from finalURL until cleanup
func mockDiscoveryGet(t *testing.T, status int, doc map[string]any, finalURL string) *cannedTransport {
	t.Helper()
	if finalURL == "" {
		finalURL = oidcIssuer + "/.well-known/openid-configuration"
	}
	rt := &cannedTransport{status: status, body: jsonText(doc), ctype: "application/json", finalURL: finalURL}
	prev := selfHostedDiscoveryHTTP
	selfHostedDiscoveryHTTP = &http.Client{Transport: rt, CheckRedirect: pyhttpx.RedirectPolicy(true)}
	t.Cleanup(func() { selfHostedDiscoveryHTTP = prev })
	return rt
}

func oidcDiscoveryDoc() map[string]any {
	return map[string]any{"issuer": oidcIssuer, "authorization_endpoint": oidcIssuer + "/authorize",
		"token_endpoint": oidcIssuer + "/token", "jwks_uri": oidcIssuer + "/jwks", "revocation_endpoint": oidcIssuer + "/revoke"}
}

func newOIDCProvider(t *testing.T, issuer string) *SelfHostedOIDCProvider {
	t.Helper()
	p, err := NewSelfHostedOIDCProvider(issuer, oidcClientID, selfHostedDefaultScopes, "", DefaultTokenLeewaySeconds)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantProviderError(t *testing.T, err error, fragment string) {
	t.Helper()
	var providerErr *gateway.ProviderError
	if !errors.As(err, &providerErr) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("want ProviderError with %q, got %v", fragment, err)
	}
}

func completeOIDC(t *testing.T, p *SelfHostedOIDCProvider, code, verifier string) (*gateway.DashboardSession, error) {
	t.Helper()
	return p.CompleteLogin(context.Background(), code, "s", verifier, "https://hermes.example/auth/callback")
}

// test_self_hosted_provider.py TestConstruction test_protocol_compliance
func TestConstructionProtocolCompliance(t *testing.T) {
	basicCase(t, "")
	if err := gateway.AssertDashboardAuthProtocol(newOIDCProvider(t, oidcIssuer)); err != nil {
		t.Fatal(err)
	}
}

// test_self_hosted_provider.py TestConstruction test_requires_issuer
func TestConstructionRequiresIssuer(t *testing.T) {
	_, err := NewSelfHostedOIDCProvider("", oidcClientID, "", "", DefaultTokenLeewaySeconds)
	var valueErr *gateway.DashboardAuthValueError
	if !errors.As(err, &valueErr) || !strings.Contains(err.Error(), "issuer") {
		t.Fatal(err)
	}
}

// test_self_hosted_provider.py TestConstruction test_rejects_non_https_issuer
func TestConstructionRejectsNonHTTPSIssuer(t *testing.T) {
	_, err := NewSelfHostedOIDCProvider("http://auth.example.com", oidcClientID, "", "", DefaultTokenLeewaySeconds)
	wantProviderError(t, err, "https")
}

// test_self_hosted_provider.py TestDiscovery test_fetches_and_caches
func TestDiscoveryFetchesAndCaches(t *testing.T) {
	p := newOIDCProvider(t, oidcIssuer)
	rt := mockDiscoveryGet(t, 200, oidcDiscoveryDoc(), "")
	d1, err := p.getDiscovery()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := p.getDiscovery()
	if err != nil {
		t.Fatal(err)
	}
	if d1.tokenEndpoint != oidcIssuer+"/token" || d1.authorizationEndpoint != oidcIssuer+"/authorize" ||
		d1.jwksURI != oidcIssuer+"/jwks" || d1.revocationEndpoint != oidcIssuer+"/revoke" {
		t.Fatalf("%+v", d1)
	}
	if len(rt.calls) != 1 || d1 != d2 {
		t.Fatalf("calls %d same %v", len(rt.calls), d1 == d2)
	}
}

// test_self_hosted_provider.py TestDiscovery test_redirect_landing_off_origin_rejected
func TestDiscoveryRedirectLandingOffOriginRejected(t *testing.T) {
	p := newOIDCProvider(t, oidcIssuer)
	forged := oidcDiscoveryDoc()
	forged["jwks_uri"], forged["token_endpoint"] = "https://attacker.example/jwks", "https://attacker.example/token"
	mockDiscoveryGet(t, 200, forged, "https://attacker.example/openid-configuration")
	_, err := p.fetchDiscovery()
	wantProviderError(t, err, "origin")
}

// test_self_hosted_provider.py TestDiscovery test_redirect_landing_on_cleartext_rejected
func TestDiscoveryRedirectLandingOnCleartextRejected(t *testing.T) {
	p := newOIDCProvider(t, oidcIssuer)
	mockDiscoveryGet(t, 200, oidcDiscoveryDoc(), "http://auth.example.com/discovery")
	_, err := p.fetchDiscovery()
	wantProviderError(t, err, "origin")
}

// test_self_hosted_provider.py TestDiscovery test_same_origin_redirect_allowed
func TestDiscoverySameOriginRedirectAllowed(t *testing.T) {
	p := newOIDCProvider(t, oidcIssuer)
	mockDiscoveryGet(t, 200, oidcDiscoveryDoc(), "https://auth.example.com/.well-known/openid-configuration/application/o/hermes")
	d, err := p.fetchDiscovery()
	if err != nil || d.tokenEndpoint != oidcIssuer+"/token" {
		t.Fatalf("%+v %v", d, err)
	}
}

// test_self_hosted_provider.py TestDiscovery test_explicit_default_port_is_same_origin
func TestDiscoveryExplicitDefaultPortIsSameOrigin(t *testing.T) {
	p := newOIDCProvider(t, oidcIssuer)
	mockDiscoveryGet(t, 200, oidcDiscoveryDoc(), "https://auth.example.com:443/x")
	d, err := p.fetchDiscovery()
	if err != nil || d.issuer != oidcIssuer {
		t.Fatalf("%+v %v", d, err)
	}
}

// Loopback server answering each path with its status, headers and body
func serveRoutes(t *testing.T) (*httptest.Server, map[string][3]any) {
	t.Helper()
	routes := map[string][3]any{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		route, ok := routes[r.URL.Path]
		mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			return
		}
		for k, v := range route[1].(map[string]string) {
			w.Header().Set(k, v)
		}
		w.WriteHeader(route[0].(int))
		_, _ = w.Write(route[2].([]byte))
	}))
	t.Cleanup(srv.Close)
	return srv, routes
}

// test_self_hosted_provider.py TestDiscoveryRealRedirect test_real_same_origin_redirect_succeeds
func TestDiscoveryRealRedirectRealSameOriginRedirectSucceeds(t *testing.T) {
	srv, routes := serveRoutes(t)
	issuer := srv.URL
	doc := []byte(jsonText(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
		"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks"}))
	routes["/.well-known/openid-configuration"] = [3]any{302, map[string]string{"Location": issuer + "/canonical"}, []byte{}}
	routes["/canonical"] = [3]any{200, map[string]string{"Content-Type": "application/json"}, doc}
	d, err := newOIDCProvider(t, issuer).fetchDiscovery()
	if err != nil || d.issuer != issuer {
		t.Fatalf("%+v %v", d, err)
	}
}

// Redirecting issuer whose discovery lands on a second loopback origin serving a forged document
func forgedRedirectIssuer(t *testing.T) string {
	t.Helper()
	forge, forgeRoutes := serveRoutes(t)
	redirect, redirectRoutes := serveRoutes(t)
	issuer := redirect.URL
	forgeRoutes["/doc"] = [3]any{200, map[string]string{"Content-Type": "application/json"}, []byte(jsonText(map[string]any{
		"issuer": issuer, "authorization_endpoint": "https://attacker.example/authorize",
		"token_endpoint": "https://attacker.example/token", "jwks_uri": "https://attacker.example/jwks"}))}
	redirectRoutes["/.well-known/openid-configuration"] = [3]any{302, map[string]string{"Location": forge.URL + "/doc"}, []byte{}}
	return issuer
}

// test_self_hosted_provider.py TestDiscoveryRealRedirect test_real_redirect_to_other_origin_rejected
func TestDiscoveryRealRedirectRealRedirectToOtherOriginRejected(t *testing.T) {
	_, err := newOIDCProvider(t, forgedRedirectIssuer(t)).fetchDiscovery()
	wantProviderError(t, err, "origin")
}

// test_self_hosted_provider.py TestDiscoveryRealRedirect test_start_login_rejects_forged_discovery_through_real_redirect
func TestDiscoveryRealRedirectStartLoginRejectsForgedDiscoveryThroughRealRedirect(t *testing.T) {
	_, err := newOIDCProvider(t, forgedRedirectIssuer(t)).StartLogin(context.Background(), "https://dash.example.com/auth/callback")
	wantProviderError(t, err, "origin")
}

func authorizeParams(t *testing.T, redirect string) url.Values {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// test_self_hosted_provider.py TestStartLogin test_authorize_url_has_required_params
func TestStartLoginAuthorizeURLHasRequiredParams(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	result, err := p.StartLogin(context.Background(), "https://hermes.example/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	q := authorizeParams(t, result.RedirectURL)
	if q.Get("response_type") != "code" || q.Get("client_id") != oidcClientID || q.Get("redirect_uri") != "https://hermes.example/auth/callback" ||
		q.Get("scope") != "openid profile email" || q.Get("code_challenge_method") != "S256" || !q.Has("state") || !q.Has("code_challenge") {
		t.Fatal(q)
	}
}

// test_self_hosted_provider.py TestStartLogin test_state_in_cookie_matches_url
func TestStartLoginStateInCookieMatchesURL(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	result, err := p.StartLogin(context.Background(), "https://hermes.example/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string]string{}
	for _, seg := range strings.Split(result.CookiePayload["hermes_session_pkce"], ";") {
		if k, v, ok := strings.Cut(seg, "="); ok {
			parts[k] = v
		}
	}
	if parts["state"] != authorizeParams(t, result.RedirectURL).Get("state") {
		t.Fatal(parts)
	}
}

// test_self_hosted_provider.py TestCompleteLogin test_happy_path_returns_session
func TestCompleteLoginHappyPathReturnsSession(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	idToken := mintIDToken(t, 900, nil)
	mockTokenPost(t, 200, jsonText(map[string]any{"access_token": "opaque-at", "id_token": idToken, "token_type": "Bearer",
		"refresh_token": "rt_initial"}))
	s, err := completeOIDC(t, p, "abc", "vfy")
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != "usr_abc" || s.Provider != "self-hosted" || s.Email != "alice@example.com" || s.DisplayName != "Alice Example" ||
		s.AccessToken != idToken || s.RefreshToken != "rt_initial" {
		t.Fatalf("%+v", s)
	}
}

// test_self_hosted_provider.py TestCompleteLogin test_tolerates_missing_refresh_token
func TestCompleteLoginToleratesMissingRefreshToken(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	mockTokenPost(t, 200, jsonText(map[string]any{"id_token": mintIDToken(t, 900, nil), "token_type": "Bearer"}))
	s, err := completeOIDC(t, p, "abc", "vfy")
	if err != nil || s.RefreshToken != "" {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_self_hosted_provider.py TestCompleteLogin test_missing_id_token_raises
func TestCompleteLoginMissingIDTokenRaises(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	mockTokenPost(t, 200, jsonText(map[string]any{"access_token": "opaque", "token_type": "Bearer"}))
	_, err := completeOIDC(t, p, "x", "v")
	wantProviderError(t, err, "id_token")
}

// test_self_hosted_provider.py TestCompleteLogin test_400_raises_invalid_code
func TestCompleteLogin400RaisesInvalidCode(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	mockTokenPost(t, 400, jsonText(map[string]any{"error": "invalid_grant"}))
	_, err := completeOIDC(t, p, "bad", "v")
	var invalid *gateway.InvalidCodeError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatal(err)
	}
}

// Completes a login against a recorded token endpoint and returns the request it saw
func completeRecorded(t *testing.T, p *SelfHostedOIDCProvider) (*http.Request, url.Values) {
	t.Helper()
	rt := mockTokenPost(t, 200, jsonText(map[string]any{"id_token": mintIDToken(t, 900, nil), "token_type": "Bearer",
		"refresh_token": "rt_initial"}))
	if _, err := completeOIDC(t, p, "the-code", "the-verifier"); err != nil {
		t.Fatal(err)
	}
	return rt.calls[len(rt.calls)-1], rt.forms[len(rt.forms)-1]
}

func decodeBasic(t *testing.T, header string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(header, "Basic ") {
		t.Fatal(header)
	}
	raw, err := base64.StdEncoding.DecodeString(header[len("Basic "):])
	if err != nil {
		t.Fatal(err)
	}
	user, pw, _ := strings.Cut(string(raw), ":")
	u, _ := url.PathUnescape(user)
	p, _ := url.PathUnescape(pw)
	return u, p
}

// test_self_hosted_provider.py TestConfidentialClient test_public_client_sends_no_secret_or_auth_header
func TestConfidentialClientPublicClientSendsNoSecretOrAuthHeader(t *testing.T) {
	req, form := completeRecorded(t, makeOIDCProvider(t, "", "", nil))
	if req.Header.Get("Authorization") != "" || form.Has("client_secret") || form.Get("code_verifier") != "the-verifier" {
		t.Fatalf("%v %v", req.Header, form)
	}
	headers := req.Header.Clone()
	headers.Del("Content-Type")
	if len(headers) != 1 || headers.Get("Accept") != "application/json" {
		t.Fatal(headers)
	}
}

// test_self_hosted_provider.py TestConfidentialClient test_confidential_defaults_to_basic_when_methods_absent
func TestConfidentialClientConfidentialDefaultsToBasicWhenMethodsAbsent(t *testing.T) {
	req, form := completeRecorded(t, makeOIDCProvider(t, "", "s3cr3t", nil))
	if form.Has("client_secret") || form.Get("code_verifier") != "the-verifier" {
		t.Fatal(form)
	}
	if user, pw := decodeBasic(t, req.Header.Get("Authorization")); user != oidcClientID || pw != "s3cr3t" {
		t.Fatal(user, pw)
	}
}

// test_self_hosted_provider.py TestConfidentialClient test_basic_url_encodes_reserved_chars_in_secret
func TestConfidentialClientBasicURLEncodesReservedCharsInSecret(t *testing.T) {
	tricky := "p@ss:wo rd/+="
	req, _ := completeRecorded(t, makeOIDCProvider(t, "", tricky, []string{"client_secret_basic"}))
	if user, pw := decodeBasic(t, req.Header.Get("Authorization")); user != oidcClientID || pw != tricky {
		t.Fatal(user, pw)
	}
}

// test_self_hosted_provider.py TestConfidentialClient test_refresh_grant_authenticates_confidential_client
func TestConfidentialClientRefreshGrantAuthenticatesConfidentialClient(t *testing.T) {
	p := makeOIDCProvider(t, "", "s3cr3t", []string{"client_secret_post"})
	rt := mockTokenPost(t, 200, jsonText(map[string]any{"id_token": mintIDToken(t, 900, nil), "token_type": "Bearer", "refresh_token": "rt2"}))
	if _, err := p.RefreshSession(context.Background(), "rt_old"); err != nil {
		t.Fatal(err)
	}
	form := rt.forms[len(rt.forms)-1]
	if form.Get("grant_type") != "refresh_token" || form.Get("client_secret") != "s3cr3t" {
		t.Fatal(form)
	}
}

// test_self_hosted_provider.py TestConfidentialClient test_secret_not_in_repr_or_log
func TestConfidentialClientSecretNotInReprOrLog(t *testing.T) {
	records := captureSlog(t)
	p := makeOIDCProvider(t, "", "sup3r-s3cr3t", nil)
	if strings.Contains(pyrepr.Repr(p), "sup3r-s3cr3t") {
		t.Fatal(pyrepr.Repr(p))
	}
	for _, r := range records() {
		if strings.Contains(r.Message, "sup3r-s3cr3t") {
			t.Fatal(r.Message)
		}
	}
}

// test_self_hosted_provider.py TestVerifySession test_expired_returns_none
func TestVerifySessionExpiredReturnsNone(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	s, err := p.VerifySession(context.Background(), mintIDToken(t, -600, nil))
	if s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_self_hosted_provider.py TestVerifySession test_wrong_audience_raises
func TestVerifySessionWrongAudienceRaises(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	_, err := p.VerifySession(context.Background(), mintIDToken(t, 900, map[string]any{"aud": "some-other-client"}))
	wantProviderError(t, err, "verification failed")
}

// test_self_hosted_provider.py TestVerifySession test_failure_message_surfaces_claims
func TestVerifySessionFailureMessageSurfacesClaims(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	_, err := p.VerifySession(context.Background(), mintIDToken(t, 900, map[string]any{"iss": "https://evil.example"}))
	wantProviderError(t, err, "'https://evil.example'")
	wantProviderError(t, err, "'"+oidcIssuer+"'")
}

// test_self_hosted_provider.py TestVerifySession test_jwks_unreachable_raises
func TestVerifySessionJWKSUnreachableRaises(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	p.setJWKSClient(makeJWKSClient(dead.URL + "/jwks"))
	_, err := p.VerifySession(context.Background(), mintIDToken(t, 900, nil))
	wantProviderError(t, err, "JWKS")
}

// Home with an optional dashboard.oauth block and the OIDC env cleared
func oidcRegisterCase(t *testing.T, oauthBlock string) {
	t.Helper()
	cfg := ""
	if oauthBlock != "" {
		cfg = "dashboard:\n  oauth:\n" + oauthBlock
	}
	basicCase(t, cfg)
	for _, name := range []string{"ICEBATH_DASHBOARD_OIDC_ISSUER", "ICEBATH_DASHBOARD_OIDC_CLIENT_ID", "ICEBATH_DASHBOARD_OIDC_SCOPES",
		"ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET"} {
		t.Setenv(name, "")
	}
}

func registerSelfHosted(t *testing.T) *SelfHostedOIDCProvider {
	t.Helper()
	pctx := pluginContextFor("self-hosted", "dashboard_auth/self_hosted")
	if err := (SelfHostedPlugin{}).Register(pctx); err != nil {
		t.Fatal(err)
	}
	p, _ := gateway.SnapshotDashboardAuthRegistration("self-hosted", "").(*SelfHostedOIDCProvider)
	return p
}

// test_self_hosted_provider.py TestPluginRegister test_skips_when_unconfigured
func TestPluginRegisterSkipsWhenUnconfigured(t *testing.T) {
	oidcRegisterCase(t, "")
	if registerSelfHosted(t) != nil {
		t.Fatal("registered")
	}
	if r := SelfHostedLastSkipReason(); !strings.Contains(r, "ICEBATH_DASHBOARD_OIDC_ISSUER") || !strings.Contains(r, "self_hosted") {
		t.Fatal(r)
	}
}

// test_self_hosted_provider.py TestPluginRegister test_registers_from_env
func TestPluginRegisterRegistersFromEnv(t *testing.T) {
	oidcRegisterCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_OIDC_ISSUER", oidcIssuer)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_ID", oidcClientID)
	p := registerSelfHosted(t)
	if p == nil || p.issuer != oidcIssuer || p.clientID != oidcClientID || p.scopes != "openid profile email" || SelfHostedLastSkipReason() != "" {
		t.Fatalf("%+v %q", p, SelfHostedLastSkipReason())
	}
}

// test_self_hosted_provider.py TestPluginRegister test_env_overrides_config
func TestPluginRegisterEnvOverridesConfig(t *testing.T) {
	oidcRegisterCase(t, "    self_hosted:\n      issuer: https://config.example\n      client_id: config-client\n")
	t.Setenv("ICEBATH_DASHBOARD_OIDC_ISSUER", oidcIssuer)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_ID", oidcClientID)
	p := registerSelfHosted(t)
	if p == nil || p.issuer != oidcIssuer || p.clientID != oidcClientID {
		t.Fatalf("%+v", p)
	}
}

// test_self_hosted_provider.py TestPluginRegister test_config_load_failure_falls_through
func TestPluginRegisterConfigLoadFailureFallsThrough(t *testing.T) {
	oidcRegisterCase(t, "")
	if err := os.Mkdir(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if registerSelfHosted(t) != nil {
		t.Fatal("registered")
	}
}

// test_self_hosted_provider.py TestPluginRegister test_secret_from_env
func TestPluginRegisterSecretFromEnv(t *testing.T) {
	oidcRegisterCase(t, "")
	t.Setenv("ICEBATH_DASHBOARD_OIDC_ISSUER", oidcIssuer)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_ID", oidcClientID)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "env-secret")
	if p := registerSelfHosted(t); p == nil || p.clientSecret != "env-secret" {
		t.Fatalf("%+v", p)
	}
}

const oidcSecretBlock = "    self_hosted:\n      issuer: " + oidcIssuer + "\n      client_id: " + oidcClientID + "\n      client_secret: cfg-secret\n"

// test_self_hosted_provider.py TestPluginRegister test_env_secret_overrides_config
func TestPluginRegisterEnvSecretOverridesConfig(t *testing.T) {
	oidcRegisterCase(t, oidcSecretBlock)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "env-secret")
	if p := registerSelfHosted(t); p == nil || p.clientSecret != "env-secret" {
		t.Fatalf("%+v", p)
	}
}

// test_self_hosted_provider.py TestPluginRegister test_empty_env_secret_does_not_shadow_config
func TestPluginRegisterEmptyEnvSecretDoesNotShadowConfig(t *testing.T) {
	oidcRegisterCase(t, oidcSecretBlock)
	t.Setenv("ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "")
	if p := registerSelfHosted(t); p == nil || p.clientSecret != "cfg-secret" {
		t.Fatalf("%+v", p)
	}
}

// test_self_hosted_provider.py TestForgedSignature test_tampered_signature_is_invalid_token_not_unreachable
func TestForgedSignatureTamperedSignatureIsInvalidTokenNotUnreachable(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	parts := strings.Split(mintIDToken(t, 900, nil), ".")
	tail := "AAAA"
	if strings.HasSuffix(parts[2], "AAAA") {
		tail = "BBBB"
	}
	forged := parts[0] + "." + parts[1] + "." + parts[2][:len(parts[2])-4] + tail
	if s, err := p.VerifySession(context.Background(), forged); s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_self_hosted_provider.py TestForgedSignature test_wrong_key_signature_is_invalid_token_not_unreachable
func TestForgedSignatureWrongKeySignatureIsInvalidTokenNotUnreachable(t *testing.T) {
	p := makeOIDCProvider(t, "", "", nil)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	token := signRS256(t, other, map[string]any{"iss": oidcIssuer, "aud": oidcClientID, "sub": "usr_abc", "iat": now, "exp": now + 900})
	if s, err := p.VerifySession(context.Background(), token); s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}
