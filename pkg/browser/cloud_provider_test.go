package browser

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Strips every browser provider env var and registers the providers the bundled plugins register
func bundledProviders(t *testing.T) *tools.Env {
	for _, k := range []string{"BROWSERBASE_API_KEY", "BROWSERBASE_PROJECT_ID", "BROWSERBASE_BASE_URL", "BROWSER_USE_API_KEY",
		"BROWSER_USE_GATEWAY_URL", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "FIRECRAWL_BROWSER_TTL", "TOOL_GATEWAY_DOMAIN",
		"TOOL_GATEWAY_USER_TOKEN", "BROWSERBASE_PROXIES", "BROWSERBASE_KEEP_ALIVE", "BROWSERBASE_SESSION_TIMEOUT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	BrowserProviderRegistry.ResetForTests()
	t.Cleanup(BrowserProviderRegistry.ResetForTests)
	for _, p := range bundledBrowserPlugins {
		if err := BrowserProviderRegistry.Register(p.provider(), ""); err != nil {
			t.Fatal(err)
		}
	}
	return testEnv(t)
}

func registered(t *testing.T, name string) BrowserProvider {
	p, ok := BrowserProviderRegistry.Merged(context.Background(), "").Get(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	return p
}

// TestIsAvailable test_browserbase_requires_both_api_key_and_project_id
func TestBrowserbaseRequiresBothKeyAndProject(t *testing.T) {
	env := bundledProviders(t)
	p := registered(t, "browserbase")
	if p.IsAvailable(env) {
		t.Fatal("available without credentials")
	}
	t.Setenv("BROWSERBASE_API_KEY", "key")
	if p.IsAvailable(env) {
		t.Fatal("available with the key alone")
	}
	t.Setenv("BROWSERBASE_PROJECT_ID", "proj")
	if !p.IsAvailable(env) {
		t.Fatal("unavailable with both")
	}
}

// TestIsAvailable test_browser_use_satisfied_by_api_key and test_firecrawl_requires_api_key
func TestBrowserUseAndFirecrawlNeedTheirKey(t *testing.T) {
	env := bundledProviders(t)
	for name, key := range map[string]string{"browser-use": "BROWSER_USE_API_KEY", "firecrawl": "FIRECRAWL_API_KEY"} {
		p := registered(t, name)
		if p.IsAvailable(env) {
			t.Fatalf("%s available without %s", name, key)
		}
		t.Setenv(key, "key")
		if !p.IsAvailable(env) {
			t.Fatalf("%s unavailable with %s", name, key)
		}
	}
}

// TestRegistryResolution none without credentials, local short circuit, browser-use preferred over browserbase
func TestBundledRegistryResolution(t *testing.T) {
	env := bundledProviders(t)
	if p := resolveBrowserProvider(context.Background(), env, ""); p != nil {
		t.Fatalf("resolved %s without credentials", p.Name())
	}
	if p := resolveBrowserProvider(context.Background(), env, "local"); p != nil {
		t.Fatal("local resolved a provider")
	}
	t.Setenv("BROWSER_USE_API_KEY", "k1")
	t.Setenv("BROWSERBASE_API_KEY", "k2")
	t.Setenv("BROWSERBASE_PROJECT_ID", "p")
	if p := resolveBrowserProvider(context.Background(), env, ""); p == nil || p.Name() != "browser-use" {
		t.Fatalf("resolved %v", p)
	}
}

// The three plugins are bundled backends whose manifests carry plugin.yaml's names and descriptions
func TestBrowserPluginsBundled(t *testing.T) {
	for _, want := range []string{"browser-browser-use", "browser-browserbase", "browser-firecrawl"} {
		found := false
		for _, p := range bundledBrowserPlugins {
			m := p.Manifest()
			if m.Name == want && m.Kind == "backend" && m.Source == "bundled" && strings.HasPrefix(m.Key, "browser/") {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s missing", want)
		}
	}
	var _ plugins.Plugin = bundledBrowserPlugins[0]
	if s := newBrowserbaseProvider().GetSetupSchema(); s == nil || s["badge"] != "paid" {
		t.Fatalf("browserbase setup %v", s)
	}
	if s := newBrowserUseProvider().GetSetupSchema(); s != nil {
		t.Fatal("browser-use shown in the picker")
	}
}

// A 402 drops keepAlive then proxies, the session comes back with those features off, and close posts the release
func TestBrowserbasePaidFallbackAndRelease(t *testing.T) {
	env := bundledProviders(t)
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		if r.Header.Get("X-BB-API-Key") != "key" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/v1/sessions/s1" {
			w.WriteHeader(200)
			return
		}
		if n < 3 {
			w.WriteHeader(402)
			return
		}
		io.WriteString(w, `{"id":"s1","connectUrl":"wss://bb/s1"}`)
	}))
	defer srv.Close()
	t.Setenv("BROWSERBASE_API_KEY", "key")
	t.Setenv("BROWSERBASE_PROJECT_ID", "proj")
	t.Setenv("BROWSERBASE_BASE_URL", srv.URL+"/")
	p := registered(t, "browserbase")
	info, err := p.CreateSession(env, "task")
	if err != nil {
		t.Fatal(err)
	}
	features := info["features"].(map[string]any)
	if info["bb_session_id"] != "s1" || info["cdp_url"] != "wss://bb/s1" || features["keep_alive"] != false || features["proxies"] != false {
		t.Fatalf("session %v", info)
	}
	if !strings.HasPrefix(info["session_name"].(string), "icebath_task_") || len(info["session_name"].(string)) != len("icebath_task_")+8 {
		t.Fatalf("session name %v", info["session_name"])
	}
	if _, ok := bodies[0]["keepAlive"]; !ok || len(bodies[2]) != 1 || bodies[2]["projectId"] != "proj" {
		t.Fatalf("create bodies %v", bodies)
	}
	if !p.CloseSession(env, "s1") || bodies[3]["status"] != "REQUEST_RELEASE" {
		t.Fatalf("release body %v", bodies[3])
	}
}

// Firecrawl's missing key surfaces from the headers, logged as a close exception and raised by create
func TestFirecrawlMissingKeyPaths(t *testing.T) {
	env := bundledProviders(t)
	p := registered(t, "firecrawl")
	if p.CloseSession(env, "s") {
		t.Fatal("closed without a key")
	}
	if _, err := p.CreateSession(env, "t"); err == nil || !isValueError(err) || !strings.Contains(err.Error(), "FIRECRAWL_API_KEY environment variable is required") {
		t.Fatalf("create error %v", err)
	}
}

// Managed key kept across a retryable failure only, a 409 in progress or a 5xx
func TestPendingCreateKeyPreservation(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{{502, "", true}, {409, `{"error":{"message":"Request Already In Progress"}}`, true}, {409, `{"error":"busy"}`, false}, {401, "", false}, {409, "nope", false}}
	for _, c := range cases {
		if got := shouldPreservePendingCreateKey(&pyrequests.Response{Status: c.status, Body: []byte(c.body)}); got != c.want {
			t.Fatalf("%d %s preserved %v", c.status, c.body, got)
		}
	}
	first := pendingCreateKey("t")
	if pendingCreateKey("t") != first || !strings.HasPrefix(first, "browser-use-session-create:") || len(first) != len("browser-use-session-create:")+32 {
		t.Fatalf("key %q", first)
	}
	clearPendingCreateKey("t")
	if pendingCreateKey("t") == first {
		t.Fatal("key kept after clear")
	}
	clearPendingCreateKey("t")
}
