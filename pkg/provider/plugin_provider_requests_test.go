package provider

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- tests/hermes_cli/test_plugin_provider_requests.py ----

// Loopback stand in for a provider or plugin API that records every request it receives
type loopbackAPI struct {
	mu     sync.Mutex
	seen   []loopbackRequest
	server *httptest.Server
	url    string
	origin PluginOrigin
}

type loopbackRequest struct {
	path    string
	headers http.Header
	body    []byte
}

func newLoopbackAPI(t *testing.T, status int, location string, reply string) *loopbackAPI {
	t.Helper()
	api := &loopbackAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		api.seen = append(api.seen, loopbackRequest{path: r.URL.RequestURI(), headers: r.Header.Clone(), body: body})
		api.mu.Unlock()
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(api.server.Close)
	api.url = api.server.URL
	u, _ := url.Parse(api.url)
	port, _ := strconv.Atoi(u.Port())
	api.origin = PluginOrigin{Scheme: "http", Host: "127.0.0.1", Port: port}
	return api
}

func (a *loopbackAPI) requests() []loopbackRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]loopbackRequest{}, a.seen...)
}

func providerRequestJWT() string {
	seg := func(v map[string]any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return seg(map[string]any{"alg": "none"}) + "." + seg(map[string]any{"exp": time.Now().Unix() + 86400, "sub": "fake"}) + ".sig"
}

// Adds a loopback origin to the codex allowlist for the length of the test
func allowCodexOrigin(t *testing.T, origin PluginOrigin) {
	prev := pluginProviderRequestProviders["openai-codex"]
	next := prev
	next.origins = append(append([]PluginOrigin{}, prev.origins...), origin)
	pluginProviderRequestProviders["openai-codex"] = next
	t.Cleanup(func() { pluginProviderRequestProviders["openai-codex"] = prev })
}

func providerRequestAPIs(t *testing.T) (*loopbackAPI, *loopbackAPI, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	allowed := newLoopbackAPI(t, 200, "", `{"value": "ek_ephemeral", "expires_at": 1}`)
	other := newLoopbackAPI(t, 200, "", `{"value": "ek_ephemeral", "expires_at": 1}`)
	allowCodexOrigin(t, allowed.origin)
	return allowed, other, h
}

func providerRequestSignIn(t *testing.T, h, token string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{
		"auth_mode": "chatgpt", "tokens": map[string]any{"access_token": token, "refresh_token": "fake-rt"}}}})
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Dashboard style plugin module whose mint call carries its own file as the calling frame
func providerRequestPlugin(t *testing.T, h string, declares bool) func(string) (*ProviderResponse, error) {
	t.Helper()
	name, manifest := "undeclared", "name: voice\n"
	if declares {
		name, manifest = "voice", manifest+"requires_auth: [openai-codex]\n"
	}
	root := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(filepath.Join(root, "dashboard"), 0o755); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(root, "dashboard", "plugin_api.py")
	if err := os.WriteFile(filepath.Join(root, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(module, []byte("def mint(url): ...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return func(target string) (*ProviderResponse, error) {
		return CredentialedProviderRequest("openai-codex", "POST", target, ProviderRequestOptions{
			JSON: map[string]any{"session": map[string]any{}}, Caller: []string{module}})
	}
}

func TestTokenAttachedForAllowlistedOriginAndNeverReturned(t *testing.T) {
	allowed, _, h := providerRequestAPIs(t)
	token := providerRequestJWT()
	providerRequestSignIn(t, h, token)
	result, err := providerRequestPlugin(t, h, true)(allowed.url + "/v1/realtime/client_secrets")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := result.JSON()
	if result.Status != 200 || err != nil || payload.(map[string]any)["value"] != "ek_ephemeral" {
		t.Fatalf("status=%d payload=%v err=%v", result.Status, payload, err)
	}
	seen := allowed.requests()
	if seen[0].headers.Get("Authorization") != "Bearer "+token {
		t.Fatalf("authorization %q", seen[0].headers.Get("Authorization"))
	}
	var body any
	if err := json.Unmarshal(seen[0].body, &body); err != nil || fmt.Sprint(body) != "map[session:map[]]" {
		t.Fatalf("body %s", seen[0].body)
	}
	headers, _ := json.Marshal(result.Headers)
	if strings.Contains(fmt.Sprintf("%#v", *result), token) || strings.Contains(string(headers), token) {
		t.Fatal("the response carries the token")
	}
}

func TestDisallowedOriginAndRedirectNeverCarryTheToken(t *testing.T) {
	_, other, h := providerRequestAPIs(t)
	providerRequestSignIn(t, h, providerRequestJWT())
	mint := providerRequestPlugin(t, h, true)
	_, err := mint(other.url + "/steal")
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "Refusing") {
		t.Fatalf("err=%v", err)
	}
	if len(other.requests()) != 0 {
		t.Fatal("the foreign origin was contacted")
	}
	redirecting := newLoopbackAPI(t, 302, other.url+"/steal", `{"value": "ek_ephemeral", "expires_at": 1}`)
	allowCodexOrigin(t, redirecting.origin)
	result, err := mint(redirecting.url + "/v1/realtime/client_secrets")
	redirecting.server.Close()
	if err != nil || result.Status != 302 || len(other.requests()) != 0 {
		t.Fatalf("result=%v err=%v other=%d", result, err, len(other.requests()))
	}
}

func TestUndeclaredPluginAndNonPluginCallersAreRefused(t *testing.T) {
	allowed, _, h := providerRequestAPIs(t)
	providerRequestSignIn(t, h, providerRequestJWT())
	_, err := providerRequestPlugin(t, h, false)(allowed.url)
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "requires_auth") {
		t.Fatalf("undeclared err=%v", err)
	}
	_, err = CredentialedProviderRequest("openai-codex", "GET", allowed.url, ProviderRequestOptions{})
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "installed plugin") {
		t.Fatalf("core caller err=%v", err)
	}
	if len(allowed.requests()) != 0 {
		t.Fatal("a refused caller reached the provider")
	}
}

func TestNotSignedInNamesTheSignInCommand(t *testing.T) {
	allowed, _, h := providerRequestAPIs(t)
	_, err := providerRequestPlugin(t, h, true)(allowed.url)
	if !isProviderNotSignedIn(err) || !strings.Contains(err.Error(), "icebath auth add openai-codex") {
		t.Fatalf("err=%v", err)
	}
	if len(allowed.requests()) != 0 {
		t.Fatal("a request was sent without a sign in")
	}
}
