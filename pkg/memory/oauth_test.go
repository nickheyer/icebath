package memory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// OAuth flow double answering with the profile label it was installed for
type labelFlow struct {
	label string
	fail  error
}

func (f labelFlow) StartLoopbackFlowBackground(ctx context.Context) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return map[string]any{"state": "pending", "profile": f.label}, nil
}

func (f labelFlow) FlowStatus(ctx context.Context) (any, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return map[string]any{"state": "idle", "profile": f.label}, nil
}

// Default home with a live work profile, the process home pointed at the default
func oauthHomes(t *testing.T) (string, string) {
	t.Helper()
	defaultHome := filepath.Join(t.TempDir(), ".icebath")
	workHome := filepath.Join(defaultHome, "profiles", "work")
	if err := os.MkdirAll(workHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workHome, "profile.yaml"), []byte("name: work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(home.EnvHome, defaultHome)
	return defaultHome, workHome
}

// Registers a user source provider named name for one home with its oauth flow
func registerUserFlow(t *testing.T, name, h string, flow OAuthFlow) {
	t.Helper()
	RegisterProvider(ProviderRegistration{
		Name: name, Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", name), OAuth: flow,
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) { return nil, nil },
	})
	t.Cleanup(func() { UnregisterProviders(SourceUser, h) })
}

func oauthRequest(t *testing.T, method, target string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	OAuthRouter.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s body %q", method, target, rec.Body.String())
	}
	return rec.Code, body
}

func TestOAuthRoutesLoadTheProviderFromTheRequestedProfile(t *testing.T) {
	defaultHome, workHome := oauthHomes(t)
	registerUserFlow(t, "honcho", defaultHome, labelFlow{label: "default"})
	registerUserFlow(t, "honcho", workHome, labelFlow{label: "work"})
	for _, profile := range []string{"default", "work", "default"} {
		code, body := oauthRequest(t, http.MethodGet, OAuthRoutePrefix+"/honcho/oauth/status?profile="+profile)
		if code != http.StatusOK || body["profile"] != profile || body["state"] != "idle" {
			t.Fatalf("status for %s = %d %v", profile, code, body)
		}
		code, body = oauthRequest(t, http.MethodPost, OAuthRoutePrefix+"/honcho/oauth/start?profile="+profile)
		if code != http.StatusOK || body["profile"] != profile || body["state"] != "pending" {
			t.Fatalf("start for %s = %d %v", profile, code, body)
		}
	}
}

func TestOAuthUserDirProviderFlowResolvesFromItsHome(t *testing.T) {
	defaultHome, _ := oauthHomes(t)
	registerUserFlow(t, "honcho", defaultHome, labelFlow{label: "default"})
	flow, err := resolveOAuthFlow(context.Background(), "honcho")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := flow.StartLoopbackFlowBackground(context.Background())
	if got.(map[string]any)["state"] != "pending" {
		t.Fatalf("start %v", got)
	}
}

func TestOAuthRouteErrors(t *testing.T) {
	defaultHome, _ := oauthHomes(t)
	registerUserFlow(t, "broken", defaultHome, labelFlow{label: "default", fail: errors.New("boom")})
	RegisterProvider(ProviderRegistration{Name: "plain", Source: SourceUser, Home: defaultHome,
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) { return nil, nil }})
	cases := []struct {
		method, target string
		code           int
		detail         string
	}{
		{http.MethodGet, OAuthRoutePrefix + "/missing/oauth/status", 404, "missing does not support OAuth connect"},
		{http.MethodGet, OAuthRoutePrefix + "/plain/oauth/status", 404, "plain does not support OAuth connect"},
		{http.MethodGet, OAuthRoutePrefix + "/not-ident/oauth/status", 404, "unknown memory provider 'not-ident'"},
		{http.MethodPost, OAuthRoutePrefix + "/broken/oauth/start", 500, "Failed to start broken OAuth: boom"},
		{http.MethodGet, OAuthRoutePrefix + "/broken/oauth/status", 500, "Failed to read broken OAuth status: boom"},
		{http.MethodGet, OAuthRoutePrefix + "/broken/oauth/status?profile=Bad%20Name", 400, ""},
		{http.MethodGet, OAuthRoutePrefix + "/broken/oauth/status?profile=ghost", 404, "Profile 'ghost' does not exist."},
		{http.MethodGet, OAuthRoutePrefix + "/broken/oauth/status?profile=sudo", 400, ""},
	}
	for _, c := range cases {
		code, body := oauthRequest(t, c.method, c.target)
		if code != c.code {
			t.Fatalf("%s %s = %d %v", c.method, c.target, code, body)
		}
		if c.detail != "" && body["detail"] != c.detail {
			t.Fatalf("%s %s detail %q", c.method, c.target, body["detail"])
		}
	}
	code, body := oauthRequest(t, http.MethodGet, OAuthRoutePrefix+"/broken/oauth/status?profile=sudo")
	if code != 400 || body["detail"] != "Profile name 'sudo' is reserved — it collides with either the icebath installation itself or a common system binary.  Pick a different name." {
		t.Fatalf("reserved detail %v", body)
	}
	t.Cleanup(func() { UnregisterProviders(SourceUser, defaultHome) })
}

func TestOAuthCurrentProfileLeavesTheContextUntouched(t *testing.T) {
	for _, p := range []string{"", "  ", "current", "CURRENT"} {
		ctx, err := scopeToProfile(context.Background(), p)
		if err != nil || home.Override(ctx) != "" {
			t.Fatalf("profile %q scoped to %q err %v", p, home.Override(ctx), err)
		}
	}
}

func TestOAuthPyIsIdentifier(t *testing.T) {
	for s, want := range map[string]bool{"honcho": true, "_x1": true, "1abc": false, "a-b": false, "": false, "caf" + string(rune(0xE9)): true} {
		if pyIsIdentifier(s) != want {
			t.Fatalf("isidentifier %q", s)
		}
	}
}
