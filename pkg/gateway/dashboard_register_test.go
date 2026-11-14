package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// One registration run with every host fact pinned, the portal answering with a canned status and JSON body
type registerRun struct {
	token        string
	tokenErr     error
	managed      bool
	portal       string
	env          map[string]string
	status       int
	response     map[string]any
	capturedURL  string
	capturedAuth string
	capturedBody map[string]any
	saved        map[string]string
	out          bytes.Buffer
}

var defaultRegisterResponse = map[string]any{"client_id": "agent:selfhost-1", "id": "selfhost-1", "name": "dreamy_tesla", "kind": "SELF_HOSTED",
	"custom_redirect_uri": nil, "created_at": "2026-06-04T12:00:00.000Z"}

func (r *registerRun) run(t *testing.T, ns map[string]any) int {
	t.Helper()
	if r.response == nil {
		r.response = defaultRegisterResponse
	}
	if r.status == 0 {
		r.status = 200
	}
	if r.portal == "" {
		r.portal = "https://portal.nousresearch.com"
	}
	r.saved = map[string]string{}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_DASHBOARD_PORTAL_URL", "")
	swapSeam(t, &registerIsManaged, func(context.Context, string) bool { return r.managed })
	swapSeam(t, &registerResolveAccessToken, func(context.Context, string) (string, error) { return r.token, r.tokenErr })
	swapSeam(t, &registerResolvePortal, func(string, string) string { return r.portal })
	swapSeam(t, &registerGetEnvValue, func(_ context.Context, _, key string) (string, bool, error) {
		v, ok := r.env[key]
		return v, ok, nil
	})
	swapSeam(t, &registerSaveEnvValue, func(_, key, value string) error {
		r.saved[key] = value
		return nil
	})
	swapSeam(t, &registerHTTPDo, func(req *http.Request, _ time.Duration) (*http.Response, error) {
		r.capturedURL, r.capturedAuth = req.URL.String(), req.Header.Get("Authorization")
		raw, _ := io.ReadAll(req.Body)
		r.capturedBody = map[string]any{}
		if err := json.Unmarshal(raw, &r.capturedBody); err != nil {
			t.Errorf("request body %q: %v", raw, err)
		}
		body, _ := json.Marshal(r.response)
		return &http.Response{StatusCode: r.status, Status: "status", Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: req}, nil
	})
	if ns == nil {
		ns = map[string]any{}
	}
	code, err := CmdDashboardRegister(context.Background(), ns, &r.out)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestFastFailsNotLoggedInExits1WithSetupHint(t *testing.T) {
	r := &registerRun{tokenErr: &provider.AuthError{Message: "not logged in", Provider: "nous", ReloginRequired: true}}
	if code := r.run(t, nil); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if out := r.out.String(); !strings.Contains(out, "not logged into Nous Portal") || !strings.Contains(out, "icebath setup") {
		t.Fatalf("out = %q", out)
	}
}

func TestFastFailsManagedInstallRefuses(t *testing.T) {
	r := &registerRun{managed: true}
	if code := r.run(t, nil); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(r.out.String(), "not available in a managed") {
		t.Fatalf("out = %q", r.out.String())
	}
}

func TestHappyPathWritesClientIdAndPostsGeneratedName(t *testing.T) {
	r := &registerRun{token: "tok_abc"}
	r.run(t, nil)
	if !strings.HasSuffix(r.capturedURL, "/api/oauth/self-hosted-client") || r.capturedAuth != "Bearer tok_abc" {
		t.Fatalf("url = %q auth = %q", r.capturedURL, r.capturedAuth)
	}
	if name, _ := r.capturedBody["name"].(string); name == "" {
		t.Fatalf("body = %v", r.capturedBody)
	}
	if _, sent := r.capturedBody["custom_redirect_uri"]; sent {
		t.Fatalf("body = %v", r.capturedBody)
	}
	if r.saved["ICEBATH_DASHBOARD_OAUTH_CLIENT_ID"] != "agent:selfhost-1" {
		t.Fatalf("saved = %v", r.saved)
	}
	if _, wrote := r.saved["ICEBATH_DASHBOARD_PORTAL_URL"]; wrote {
		t.Fatalf("saved = %v", r.saved)
	}
	if out := r.out.String(); !strings.Contains(out, "Registered dashboard") || !strings.Contains(out, "non-loopback bind") {
		t.Fatalf("out = %q", out)
	}
}

func TestHappyPathExplicitNameIsSent(t *testing.T) {
	r := &registerRun{token: "tok_abc"}
	r.run(t, map[string]any{"name": "my_box"})
	if r.capturedBody["name"] != "my_box" {
		t.Fatalf("body = %v", r.capturedBody)
	}
}

func TestIdempotentRerunStaleIdFallsThroughToCreatePrintsRegistered(t *testing.T) {
	r := &registerRun{token: "tok_abc", env: map[string]string{"ICEBATH_DASHBOARD_OAUTH_CLIENT_ID": "agent:selfhost-stale"},
		response: map[string]any{"client_id": "agent:selfhost-new", "id": "selfhost-new", "name": "seed_name", "kind": "SELF_HOSTED",
			"custom_redirect_uri": nil, "created_at": "2026-06-04T12:00:00.000Z"}}
	r.run(t, map[string]any{"name": "seed_name"})
	if r.capturedBody["client_id"] != "agent:selfhost-stale" {
		t.Fatalf("body = %v", r.capturedBody)
	}
	if out := r.out.String(); !strings.Contains(out, "Registered dashboard") || strings.Contains(out, "Updated dashboard") {
		t.Fatalf("out = %q", out)
	}
	if r.saved["ICEBATH_DASHBOARD_OAUTH_CLIENT_ID"] != "agent:selfhost-new" {
		t.Fatalf("saved = %v", r.saved)
	}
}

func TestIdempotentRerunBlankStoredClientIdTreatedAsFirstRun(t *testing.T) {
	r := &registerRun{token: "tok_abc", env: map[string]string{"ICEBATH_DASHBOARD_OAUTH_CLIENT_ID": "   "}}
	r.run(t, nil)
	if _, sent := r.capturedBody["client_id"]; sent {
		t.Fatalf("body = %v", r.capturedBody)
	}
	if name, _ := r.capturedBody["name"].(string); name == "" {
		t.Fatalf("body = %v", r.capturedBody)
	}
}

func TestCustomPortalPersistenceNoFlagDefaultPortalNotWritten(t *testing.T) {
	r := &registerRun{token: "tok", portal: "https://portal.nousresearch.com"}
	r.run(t, nil)
	if _, wrote := r.saved["ICEBATH_DASHBOARD_PORTAL_URL"]; wrote {
		t.Fatalf("saved = %v", r.saved)
	}
}

func TestPublicUrlPersistenceNoRedirectFlagDoesNotOverwriteExisting(t *testing.T) {
	r := &registerRun{token: "tok", env: map[string]string{"ICEBATH_DASHBOARD_PUBLIC_URL": "https://already-set.example.com"}}
	r.run(t, nil)
	if _, wrote := r.saved["ICEBATH_DASHBOARD_PUBLIC_URL"]; wrote {
		t.Fatalf("saved = %v", r.saved)
	}
}

func TestPublicUrlPersistenceNonHttpRedirectNotPersisted(t *testing.T) {
	r := &registerRun{token: "tok"}
	r.run(t, map[string]any{"redirect_uri": "not-a-url"})
	if _, wrote := r.saved["ICEBATH_DASHBOARD_PUBLIC_URL"]; wrote {
		t.Fatalf("saved = %v", r.saved)
	}
}

func TestPublicUrlPersistencePublicUrlPersistedAlongsidePortalUrl(t *testing.T) {
	r := &registerRun{token: "tok", portal: "https://preview.example.com",
		response: map[string]any{"client_id": "agent:selfhost-1", "id": "selfhost-1", "name": "dreamy_tesla", "kind": "SELF_HOSTED",
			"custom_redirect_uri": "https://icebath.example.com/auth/callback", "created_at": "2026-06-04T12:00:00.000Z"}}
	r.run(t, map[string]any{"portal_url": "https://preview.example.com", "redirect_uri": "https://icebath.example.com/auth/callback"})
	if r.saved["ICEBATH_DASHBOARD_PORTAL_URL"] != "https://preview.example.com" || r.saved["ICEBATH_DASHBOARD_PUBLIC_URL"] != "https://icebath.example.com" {
		t.Fatalf("saved = %v", r.saved)
	}
}

func TestPortalResolutionOverrideArgWins(t *testing.T) {
	if got := resolvePortalBaseURL(t.TempDir(), "https://preview.example.com/"); got != "https://preview.example.com" {
		t.Fatalf("portal = %q", got)
	}
}

func TestPortalResolutionFallsBackToStoredLoginPortal(t *testing.T) {
	swapSeam(t, &registerNousAuthState, func(string) (map[string]any, error) {
		return map[string]any{"portal_base_url": "https://portal.staging-nousresearch.com"}, nil
	})
	if got := resolvePortalBaseURL(t.TempDir(), ""); got != "https://portal.staging-nousresearch.com" {
		t.Fatalf("portal = %q", got)
	}
}

func TestPortalErrors401MapsToReauthMessage(t *testing.T) {
	r := &registerRun{token: "tok", status: 401, response: map[string]any{"error": "invalid_token"}}
	if code := r.run(t, nil); code != 1 {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(r.out.String(), "re-authenticate") {
		t.Fatalf("out = %q", r.out.String())
	}
}
