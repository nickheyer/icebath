package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/builtin/dashboardauth"
	"github.com/nickheyer/icebath/pkg/gateway"
)

const opaquePeerKey = "hk_live_opaque_peer_key_0123456789abcdef"

// Gated application on agent.example.test with a nous provider whose portal serves an empty JWKS
func gatedNousClient(t *testing.T) *testClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"keys": []}`))
	}))
	t.Cleanup(srv.Close)
	app := dashboardTestApp(t)
	gateway.ClearDashboardAuthProviders()
	t.Cleanup(gateway.ClearDashboardAuthProviders)
	for k, v := range map[string]any{"bound_host": "agent.example.test", "bound_port": 443, "auth_required": true} {
		prev := app.State.Get(k)
		app.State.Set(k, v)
		t.Cleanup(func() { app.State.Set(k, prev) })
	}
	p, err := dashboardauth.NewNousDashboardAuthProvider("agent:test-instance", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.RegisterDashboardAuthProvider(context.Background(), p, ""); err != nil {
		t.Fatal(err)
	}
	return newTestClientAt(t, app, "https", "agent.example.test", 443)
}

// test_opaque_bearer_not_unreachable.py test_gated_api_rejects_opaque_bearer_with_401_not_503
func TestGatedAPIRejectsOpaqueBearerWith401Not503(t *testing.T) {
	r := gatedNousClient(t).get("/api/auth/me", "Authorization", "Bearer "+opaquePeerKey)
	if r.Status == 503 || r.Status != 401 || strings.Contains(strings.ToLower(string(r.Body)), "unreachable") {
		t.Fatalf("%d %s %v", r.Status, r.Body, r.AppErr)
	}
}

// test_opaque_bearer_not_unreachable.py test_gated_api_rejects_opaque_cookie_with_401_not_503
func TestGatedAPIRejectsOpaqueCookieWith401Not503(t *testing.T) {
	r := gatedNousClient(t).get("/api/auth/me", "Cookie", SessionATCookie+"="+opaquePeerKey)
	if r.Status == 503 || strings.Contains(strings.ToLower(string(r.Body)), "unreachable") {
		t.Fatalf("%d %s %v", r.Status, r.Body, r.AppErr)
	}
}
