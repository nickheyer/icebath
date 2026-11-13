package dashboardauth

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const nousCallback = "https://hermes.fly.dev/auth/callback"

// Portal access token of the fixture key with the hermes case defaults, nil override values dropping the claim
func mintNousToken(t *testing.T, ttlSeconds int64, overrides map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{"iss": "https://portal.example.com", "aud": "agent:inst123", "sub": "usr_abc", "iat": now,
		"exp": now + ttlSeconds, "scope": "agent_dashboard:access", "agent_instance_id": "inst123", "oauth_contract_version": 1,
		"org_id": "org_xyz"}
	for k, v := range overrides {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	return signRS256(t, oidcFixtureKey(), claims)
}

func newNous(t *testing.T, clientID string) *NousDashboardAuthProvider {
	t.Helper()
	p, err := NewNousDashboardAuthProvider(clientID, "https://portal.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Provider for the inst123 agent client whose JWKS client serves the fixture key
func patchedNous(t *testing.T) *NousDashboardAuthProvider {
	t.Helper()
	p := newNous(t, "agent:inst123")
	p.setJWKSClient(makeJWKSClient(oidcJWKSURL(t)))
	return p
}

func completeNous(p *NousDashboardAuthProvider) (*gateway.DashboardSession, error) {
	return p.CompleteLogin(context.Background(), "abc", "state-val", "vfy", nousCallback)
}

func nousRegisterCase(t *testing.T, oauthBlock string) {
	t.Helper()
	cfg := ""
	if oauthBlock != "" {
		cfg = "dashboard:\n  oauth:\n" + oauthBlock
	}
	drainCase(t, cfg)
	t.Setenv(nousOAuthClientIDEnv, "")
	t.Setenv(nousPortalURLEnv, "")
}

func registerNous(t *testing.T) *NousDashboardAuthProvider {
	t.Helper()
	if err := (NousPlugin{}).Register(pluginContextFor("nous", "dashboard_auth/nous")); err != nil {
		t.Fatal(err)
	}
	p, _ := gateway.SnapshotDashboardAuthRegistration("nous", "").(*NousDashboardAuthProvider)
	return p
}

// test_nous_provider.py TestConstruction test_protocol_compliance
func TestProtocolCompliance(t *testing.T) {
	if err := gateway.AssertDashboardAuthProtocol(newNous(t, "agent:inst1")); err != nil {
		t.Fatal(err)
	}
}

// Register builds the provider with the instance id, trailing slashes trimmed, derived endpoints and leeway
func TestNousConstructionDerivesEndpoints(t *testing.T) {
	nousRegisterCase(t, "    token_leeway: 7\n")
	t.Setenv(nousOAuthClientIDEnv, "agent:inst9")
	t.Setenv(nousPortalURLEnv, "https://portal.example.com///")
	p := registerNous(t)
	if p == nil {
		t.Fatal(NousLastSkipReason())
	}
	if p.clientID != "agent:inst9" || p.agentInstanceID != "inst9" || p.portalURL != "https://portal.example.com" || p.tokenLeeway != 7 ||
		p.jwksURL != "https://portal.example.com/.well-known/jwks.json" || p.authorizeURL != "https://portal.example.com/oauth/authorize" ||
		p.tokenURL != "https://portal.example.com/api/oauth/token" || p.jwksClient != nil {
		t.Fatalf("provider %+v", p)
	}
	if _, err := NewNousDashboardAuthProvider("agent-inst9", "https://x", nil); err == nil || err.Error() != "client_id must match contract shape 'agent:{instance_id}', got 'agent-inst9'" {
		t.Fatal(err)
	}
}

// test_nous_provider.py TestConstruction test_rejects_malformed_client_id
func TestRejectsMalformedClientID(t *testing.T) {
	_, err := NewNousDashboardAuthProvider("hermes-dashboard", "https://x", nil)
	var valueErr *gateway.DashboardAuthValueError
	if !errors.As(err, &valueErr) || !strings.Contains(err.Error(), "agent:") {
		t.Fatal(err)
	}
}

// test_nous_provider.py TestPluginRegister test_skips_when_client_id_missing
func TestSkipsWhenClientIDMissing(t *testing.T) {
	nousRegisterCase(t, "")
	if registerNous(t) != nil {
		t.Fatal("registered")
	}
	if !strings.Contains(NousLastSkipReason(), "ICEBATH_DASHBOARD_OAUTH_CLIENT_ID") {
		t.Fatal(NousLastSkipReason())
	}
}

// test_nous_provider.py TestPluginRegister test_registers_with_default_portal_url_when_only_client_id_set
func TestRegistersWithDefaultPortalURLWhenOnlyClientIDSet(t *testing.T) {
	nousRegisterCase(t, "")
	t.Setenv(nousOAuthClientIDEnv, "agent:inst1")
	p := registerNous(t)
	if p == nil || p.portalURL != "https://portal.nousresearch.com" || NousLastSkipReason() != "" {
		t.Fatalf("%v %q", p, NousLastSkipReason())
	}
}

// test_nous_provider.py TestPluginRegister test_empty_portal_url_env_uses_default
func TestEmptyPortalURLEnvUsesDefault(t *testing.T) {
	nousRegisterCase(t, "")
	t.Setenv(nousOAuthClientIDEnv, "agent:inst1")
	t.Setenv(nousPortalURLEnv, "")
	if p := registerNous(t); p == nil || p.portalURL != "https://portal.nousresearch.com" {
		t.Fatalf("%v", p)
	}
}

// test_nous_provider.py TestConfigYamlSource test_config_yaml_only_client_id_registers
func TestConfigYAMLOnlyClientIDRegisters(t *testing.T) {
	nousRegisterCase(t, "    client_id: agent:from-config\n")
	p := registerNous(t)
	if p == nil || p.clientID != "agent:from-config" || p.portalURL != "https://portal.nousresearch.com" {
		t.Fatalf("%v", p)
	}
}

// test_nous_provider.py TestConfigYamlSource test_env_overrides_config_client_id
func TestEnvOverridesConfigClientID(t *testing.T) {
	nousRegisterCase(t, "    client_id: agent:from-config\n")
	t.Setenv(nousOAuthClientIDEnv, "agent:from-env")
	if p := registerNous(t); p == nil || p.clientID != "agent:from-env" {
		t.Fatalf("env var must override config.yaml: %v", p)
	}
}

func startNous(t *testing.T, p *NousDashboardAuthProvider, redirect string) gateway.LoginStart {
	t.Helper()
	result, err := p.StartLogin(context.Background(), redirect)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func pkceParts(result gateway.LoginStart) map[string]string {
	parts := map[string]string{}
	for _, seg := range strings.Split(result.CookiePayload["icebath_session_pkce"], ";") {
		if k, v, ok := strings.Cut(seg, "="); ok {
			parts[k] = v
		}
	}
	return parts
}

// test_nous_provider.py TestStartLogin test_redirect_url_targets_portal_authorize
func TestRedirectURLTargetsPortalAuthorize(t *testing.T) {
	if r := startNous(t, newNous(t, "agent:inst1"), nousCallback); !strings.HasPrefix(r.RedirectURL, "https://portal.example.com/oauth/authorize?") {
		t.Fatal(r.RedirectURL)
	}
}

// test_nous_provider.py TestStartLogin test_authorize_url_has_required_params
func TestAuthorizeURLHasRequiredParams(t *testing.T) {
	q := authorizeParams(t, startNous(t, newNous(t, "agent:inst1"), nousCallback).RedirectURL)
	if q.Get("response_type") != "code" || q.Get("client_id") != "agent:inst1" || q.Get("redirect_uri") != nousCallback ||
		q.Get("scope") != "agent_dashboard:access" || q.Get("code_challenge_method") != "S256" || !q.Has("state") || !q.Has("code_challenge") {
		t.Fatal(q)
	}
}

// test_nous_provider.py TestStartLogin test_code_verifier_in_cookie_payload_43_to_128_chars
func TestCodeVerifierInCookiePayload43To128Chars(t *testing.T) {
	r := startNous(t, newNous(t, "agent:inst1"), nousCallback)
	if _, ok := r.CookiePayload["icebath_session_pkce"]; !ok {
		t.Fatal(r.CookiePayload)
	}
	if n := len(pkceParts(r)["verifier"]); n < 43 || n > 128 {
		t.Fatal(n)
	}
}

// test_nous_provider.py TestStartLogin test_state_in_cookie_payload_matches_url_param
func TestStateInCookiePayloadMatchesURLParam(t *testing.T) {
	r := startNous(t, newNous(t, "agent:inst1"), nousCallback)
	if pkceParts(r)["state"] != authorizeParams(t, r.RedirectURL).Get("state") {
		t.Fatal(r)
	}
}

// test_nous_provider.py TestStartLogin test_two_calls_produce_different_state_and_verifier
func TestTwoCallsProduceDifferentStateAndVerifier(t *testing.T) {
	p := newNous(t, "agent:inst1")
	if startNous(t, p, nousCallback).CookiePayload["icebath_session_pkce"] == startNous(t, p, nousCallback).CookiePayload["icebath_session_pkce"] {
		t.Fatal("identical pkce payloads")
	}
}

// test_nous_provider.py TestStartLogin test_allows_http_with_arbitrary_host
func TestAllowsHTTPWithArbitraryHost(t *testing.T) {
	p := newNous(t, "agent:inst1")
	for _, redirect := range []string{"http://hermes.fly.dev/auth/callback", "http://192.168.1.50:8080/auth/callback",
		"http://my-internal-host/auth/callback"} {
		startNous(t, p, redirect)
	}
}

// test_nous_provider.py TestCompleteLogin test_happy_path_returns_session
func TestHappyPathReturnsSession(t *testing.T) {
	p := patchedNous(t)
	token := mintNousToken(t, 900, nil)
	mockTokenPost(t, 200, jsonText(map[string]any{"access_token": token, "token_type": "Bearer", "refresh_token": "rt_initial_value"}))
	s, err := completeNous(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.UserID != "usr_abc" || s.Provider != "nous" || s.AccessToken != token || s.RefreshToken != "rt_initial_value" ||
		s.OrgID != "org_xyz" || s.Email != "" || s.DisplayName != "" {
		t.Fatalf("%+v", s)
	}
}

// test_nous_provider.py TestCompleteLogin test_400_raises_invalid_code
func Test400RaisesInvalidCode(t *testing.T) {
	p := patchedNous(t)
	mockTokenPost(t, 400, jsonText(map[string]any{"error": "invalid_grant"}))
	_, err := completeNous(p)
	var invalid *gateway.InvalidCodeError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatal(err)
	}
}

// test_nous_provider.py TestCompleteLogin test_500_raises_provider_error
func Test500RaisesProviderError(t *testing.T) {
	p := patchedNous(t)
	rt := mockTokenPost(t, 500, "internal server error")
	rt.ctype = "text/plain"
	_, err := completeNous(p)
	wantProviderError(t, err, "500")
}

// test_nous_provider.py TestCompleteLogin test_missing_access_token_raises
func TestMissingAccessTokenRaises(t *testing.T) {
	p := patchedNous(t)
	mockTokenPost(t, 200, jsonText(map[string]any{"token_type": "Bearer"}))
	_, err := completeNous(p)
	wantProviderError(t, err, "access_token")
}

// test_nous_provider.py TestCompleteLogin test_unexpected_token_type_raises
func TestUnexpectedTokenTypeRaises(t *testing.T) {
	p := patchedNous(t)
	mockTokenPost(t, 200, jsonText(map[string]any{"access_token": mintNousToken(t, 900, nil), "token_type": "DPoP"}))
	_, err := completeNous(p)
	wantProviderError(t, err, "token_type")
}

// test_nous_provider.py TestCompleteLogin test_network_error_raises_provider_error
func TestNetworkErrorRaisesProviderError(t *testing.T) {
	p := patchedNous(t)
	rt := mockTokenPost(t, 200, "")
	rt.fail = errors.New("conn refused")
	_, err := completeNous(p)
	wantProviderError(t, err, "unreachable")
}

func verifyNous(t *testing.T, overrides map[string]any, ttl int64) (*gateway.DashboardSession, error) {
	t.Helper()
	return patchedNous(t).VerifySession(context.Background(), mintNousToken(t, ttl, overrides))
}

// test_nous_provider.py TestVerifySession test_expired_token_returns_none
func TestExpiredTokenReturnsNone(t *testing.T) {
	if s, err := verifyNous(t, nil, -600); s != nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
}

// test_nous_provider.py TestVerifySession test_wrong_audience_raises_provider_error
func TestWrongAudienceRaisesProviderError(t *testing.T) {
	_, err := verifyNous(t, map[string]any{"aud": "agent:other-instance"}, 900)
	wantProviderError(t, err, "verification failed")
}

// test_nous_provider.py TestVerifySession test_verification_failure_message_surfaces_token_claims
func TestVerificationFailureMessageSurfacesTokenClaims(t *testing.T) {
	_, err := verifyNous(t, map[string]any{"iss": "https://evil.example"}, 900)
	wantProviderError(t, err, "'https://evil.example'")
	wantProviderError(t, err, "'https://portal.example.com'")
}

// test_nous_provider.py TestVerifySession test_agent_instance_id_mismatch_rejected
func TestAgentInstanceIDMismatchRejected(t *testing.T) {
	_, err := verifyNous(t, map[string]any{"agent_instance_id": "some-other-id"}, 900)
	wantProviderError(t, err, "agent_instance_id mismatch")
}

// test_nous_provider.py TestVerifySession test_contract_version_missing_warns_but_succeeds
func TestContractVersionMissingWarnsButSucceeds(t *testing.T) {
	records := captureSlog(t)
	s, err := verifyNous(t, map[string]any{"oauth_contract_version": nil}, 900)
	if s == nil || err != nil {
		t.Fatalf("%+v %v", s, err)
	}
	for _, r := range records() {
		if strings.Contains(r.Message, "oauth_contract_version") {
			return
		}
	}
	t.Fatal("no oauth_contract_version warning")
}

// test_nous_provider.py TestVerifySession test_jwks_unreachable_raises_provider_error
func TestJWKSUnreachableRaisesProviderError(t *testing.T) {
	p := patchedNous(t)
	dead := httptest.NewServer(nil)
	dead.Close()
	p.setJWKSClient(makeJWKSClient(dead.URL + "/jwks"))
	_, err := p.VerifySession(context.Background(), mintNousToken(t, 900, nil))
	wantProviderError(t, err, "JWKS")
}

// test_nous_provider.py TestRefreshAndRevoke test_refresh_happy_path_returns_rotated_session
func TestRefreshHappyPathReturnsRotatedSession(t *testing.T) {
	p := patchedNous(t)
	token := mintNousToken(t, 900, nil)
	rt := mockTokenPost(t, 200, jsonText(map[string]any{"access_token": token, "token_type": "Bearer", "refresh_token": "rt_rotated_value"}))
	s, err := p.RefreshSession(context.Background(), "rt_old_value")
	if err != nil || s.AccessToken != token || s.RefreshToken != "rt_rotated_value" || s.Provider != "nous" {
		t.Fatalf("%+v %v", s, err)
	}
	form, req := rt.forms[len(rt.forms)-1], rt.calls[len(rt.calls)-1]
	if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != "agent:inst123" || form.Get("refresh_token") != "rt_old_value" ||
		req.Header.Get("x-nous-refresh-token") != "rt_old_value" {
		t.Fatalf("%v %v", form, req.Header)
	}
}
