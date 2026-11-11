package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/builtin/dashboardauth"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func urlsafeToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Gated application on bellona.ts.net whose only provider is the provisioned session token
func gatedTokenApp(t *testing.T) (*testClient, *FastAPI, string) {
	t.Helper()
	token := urlsafeToken()
	p, err := dashboardauth.NewSessionTokenProvider(token)
	if err != nil {
		t.Fatal(err)
	}
	resetTicketsForTest(t)
	surfacesCase(t, p)
	app := dashboardTestApp(t)
	for k, v := range map[string]any{"bound_host": "bellona.ts.net", "bound_port": 9119, "auth_required": true} {
		prev := app.State.Get(k)
		app.State.Set(k, v)
		t.Cleanup(func() { app.State.Set(k, prev) })
	}
	return newTestClientAt(t, app, "https", "bellona.ts.net", 443), app, token
}

func passwordProviderFor(t *testing.T) interactiveTokenSubclass {
	return interactiveTokenSubclass{surfaceToken(t), "basic", "Username & Password", true}
}

func nativeQuery(extra ...string) string {
	q := url.Values{"code_challenge": {strings.Repeat("x", 43)}, "code_challenge_method": {"S256"},
		"redirect_uri": {"http://127.0.0.1:53682/callback"}, "state": {"client-state"}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return q.Encode()
}

func nativePendingCount() int {
	nativeMu.Lock()
	defer nativeMu.Unlock()
	return len(nativePending)
}

// Upgrade auth verdict of a bare /api/ws socket from a public peer with the given query
func wsVerdict(t *testing.T, app *FastAPI, query string) (string, string) {
	t.Helper()
	scope := testScope("websocket", "GET", "/api/ws?"+query, nil)
	scope.App, scope.Client = app, &Address{"203.0.113.7", 50000}
	reason, credential, err := wsAuthReason(context.Background(), NewWebSocket(scope, nil, nil), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return reason, credential
}

// test_dashboard_auth_session_token_gate.py TestGateEngagement test_token_provider_satisfies_the_fail_closed_gate
func TestGateEngagementTokenProviderSatisfiesTheFailClosedGate(t *testing.T) {
	surfacesCase(t, surfaceToken(t))
	app := dashboardTestApp(t)
	if err := configureAuthGate(context.Background(), app, "0.0.0.0", false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if app.State.Get("auth_required") != true {
		t.Fatal(app.State.Get("auth_required"))
	}
}

// test_dashboard_auth_session_token_gate.py TestGateEngagement test_no_providers_still_fails_closed
func TestGateEngagementNoProvidersStillFailsClosed(t *testing.T) {
	surfacesCase(t)
	err := configureAuthGate(context.Background(), dashboardTestApp(t), "0.0.0.0", false, nil, nil)
	var exit *gateway.DashboardExit
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
}

// test_dashboard_auth_session_token_gate.py TestRestBearerLeg test_provisioned_token_passes_the_gate
func TestRestBearerLegProvisionedTokenPassesTheGate(t *testing.T) {
	client, _, token := gatedTokenApp(t)
	r := client.get("/api/auth/me", "Authorization", "Bearer "+token)
	if r.Status != 200 {
		t.Fatalf("%d %s %v", r.Status, r.Body, r.AppErr)
	}
	if obj, ok := r.JSON(t).(*pyjson.Object); !ok || obj.GetOr("provider", nil) != "session-token" {
		t.Fatal(string(r.Body))
	}
}

// test_dashboard_auth_session_token_gate.py TestRestBearerLeg test_foreign_token_is_401
func TestRestBearerLegForeignTokenIs401(t *testing.T) {
	client, _, _ := gatedTokenApp(t)
	if r := client.get("/api/auth/me", "Authorization", "Bearer "+urlsafeToken()); r.Status != 401 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_dashboard_auth_session_token_gate.py TestRestBearerLeg test_the_desktops_stale_copy_is_401
func TestRestBearerLegTheDesktopsStaleCopyIs401(t *testing.T) {
	client, _, token := gatedTokenApp(t)
	stale := urlsafeToken()
	if stale == token {
		t.Fatal("stale equals token")
	}
	if r := client.get("/api/auth/me", "Authorization", "Bearer "+stale); r.Status != 401 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_dashboard_auth_session_token_gate.py TestWsUpgradeLeg test_provisioned_token_is_accepted
func TestWsUpgradeLegProvisionedTokenIsAccepted(t *testing.T) {
	_, app, token := gatedTokenApp(t)
	if reason, credential := wsVerdict(t, app, url.Values{"token": {token}}.Encode()); reason != "" || credential != "token" {
		t.Fatal(reason, credential)
	}
}

// test_dashboard_auth_session_token_gate.py TestWsUpgradeLeg test_foreign_token_is_rejected
func TestWsUpgradeLegForeignTokenIsRejected(t *testing.T) {
	_, app, _ := gatedTokenApp(t)
	if reason, _ := wsVerdict(t, app, url.Values{"token": {urlsafeToken()}}.Encode()); reason != "token_invalid" {
		t.Fatal(reason)
	}
}

// test_dashboard_auth_session_token_gate.py TestWsUpgradeLeg test_no_credential_is_rejected
func TestWsUpgradeLegNoCredentialIsRejected(t *testing.T) {
	_, app, _ := gatedTokenApp(t)
	if reason, credential := wsVerdict(t, app, ""); reason != "no_credential" || credential != "none" {
		t.Fatal(reason, credential)
	}
}

// test_dashboard_auth_session_token_gate.py TestNativeAuthorize test_unnamed_request_auto_selects_the_one_interactive_provider
func TestNativeAuthorizeUnnamedRequestAutoSelectsTheOneInteractiveProvider(t *testing.T) {
	client, _, _ := gatedTokenApp(t)
	if err := gateway.RegisterGlobalDashboardAuthProvider(passwordProviderFor(t)); err != nil {
		t.Fatal(err)
	}
	r := client.get("/auth/native/authorize?" + nativeQuery())
	if r.Status != 302 || !strings.HasSuffix(r.Headers.Get("location"), "/login") {
		t.Fatalf("%d %s %v", r.Status, r.Body, r.Headers)
	}
}

// test_dashboard_auth_session_token_gate.py TestNativeAuthorize test_token_only_deployment_never_reaches_start_login
func TestNativeAuthorizeTokenOnlyDeploymentNeverReachesStartLogin(t *testing.T) {
	client, _, _ := gatedTokenApp(t)
	before := nativePendingCount()
	r := client.get("/auth/native/authorize?" + nativeQuery())
	if r.Status < 400 || r.Status >= 500 || nativePendingCount() != before {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_dashboard_auth_session_token_gate.py TestNativeAuthorize test_explicitly_named_token_provider_is_rejected_before_pending_state
func TestNativeAuthorizeExplicitlyNamedTokenProviderIsRejectedBeforePendingState(t *testing.T) {
	client, _, _ := gatedTokenApp(t)
	before := nativePendingCount()
	r := client.get("/auth/native/authorize?" + nativeQuery("provider", "session-token"))
	if r.Status != 400 || nativePendingCount() != before {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_dashboard_auth_session_token_gate.py TestNativeAuthorize test_browser_login_route_rejects_the_token_provider
func TestNativeAuthorizeBrowserLoginRouteRejectsTheTokenProvider(t *testing.T) {
	client, _, _ := gatedTokenApp(t)
	if r := client.get("/auth/login?provider=session-token"); r.Status != 404 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}
