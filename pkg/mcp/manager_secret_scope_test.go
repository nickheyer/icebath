package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const scopeTokenName, scopeTokenValue = "EXAMPLE_TOKEN", "profile-own-value"

// Profile home whose env file holds the source tagged credential, routed and multiplexed for the test
func secretScopeProfile(t *testing.T) (string, context.Context) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, dir, scopeTokenName+"="+scopeTokenValue+"\n")
	tagSecretSource(t)
	multiplexOn(t)
	return dir, home.WithOverride(context.Background(), dir)
}

// Writes the profile env file
func writeEnvFile(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Tags the token as supplied by an external secret source so the child env forwards it
func tagSecretSource(t *testing.T) {
	prev := secretSourceNamesFn
	secretSourceNamesFn = func() []string { return []string{scopeTokenName} }
	t.Cleanup(func() { secretSourceNamesFn = prev })
}

// Turns profile multiplexing on for the test
func multiplexOn(t *testing.T) {
	tools.SetMultiplexActive(true)
	t.Cleanup(func() { tools.SetMultiplexActive(false) })
}

// Streamable HTTP MCP server recording every Authorization header it receives
func headerRecordingServer(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	inner := jsonMCPHandler(&atomic.Int64{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// Remote entry whose Authorization header references the token
func remoteEntry(url string) map[string]any {
	return map[string]any{"url": url, "headers": map[string]any{"Authorization": "Bearer ${" + scopeTokenName + "}"}}
}

// Environment the stdio child of srv was spawned with
func childEnviron(t *testing.T, srv *serverTask) map[string]string {
	t.Helper()
	res, err := srv.currentSession().CallTool(context.Background(), "env", map[string]any{}, nil)
	if err != nil || len(res.Content) == 0 {
		t.Fatalf("env tool %v %+v", err, res)
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReconcileCountsAdoptedSharedConnectionAsLive(t *testing.T) {
	_, env := managerHome(t)
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dirB, "config.yaml"), []byte("mcp_servers:\n  x:\n    url: https://x/mcp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := tools.ScopeKey(dirA), tools.ScopeKey(dirB)
	key := ServerKey{Scope: a, Name: "x"}
	mcpMu.Lock()
	mcpServers[key] = newServerTask("x", env)
	serverScopeKeys[key] = a
	serverToolScopes[key] = map[string]bool{a: true, b: true}
	mcpMu.Unlock()
	t.Cleanup(func() {
		mcpMu.Lock()
		delete(mcpServers, key)
		delete(serverScopeKeys, key)
		delete(serverToolScopes, key)
		mcpMu.Unlock()
	})
	ctx := tools.WithSessionToolScope(context.Background(), dirB)
	for range 3 {
		res, err := Reconcile(ctx, env)
		if err != nil || len(res.Removed)+len(res.Added)+len(res.Pending) != 0 {
			t.Fatalf("an adopted shared connection is live for this scope, nothing to add %+v %v", res, err)
		}
	}
	mcpMu.Lock()
	_, connecting := serverConnecting[ServerKey{Scope: b, Name: "x"}]
	mcpMu.Unlock()
	if connecting {
		t.Fatal("reconcile entered discovery for an adopted connection")
	}
}

func TestConnectResolvesTheOwningProfilesSecret(t *testing.T) {
	_, env := managerHome(t)
	_, ctx := secretScopeProfile(t)
	t.Setenv(scopeTokenName, "launch-env-value")
	srv, err := connectServer(ctx, env, "demo", mockEntry(t, map[string]any{"env": map[string]any{"MOCK_MCP_TOOLS": "env"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.shutdown(context.Background())
	if got := childEnviron(t, srv)[scopeTokenName]; got != scopeTokenValue {
		t.Fatalf("child env carries %q, want the owning profile's value", got)
	}
	if currentSecretScopeHome(ctx) != "" {
		t.Fatal("the owner scope binding belongs to the run task, never the caller")
	}
}

func TestUnscopedDiscoverInterpolatesHeaderRefsUnderTheOwnersScope(t *testing.T) {
	_, env := managerHome(t)
	dir, ctx := secretScopeProfile(t)
	t.Setenv(scopeTokenName, "launch-env-value")
	remote, headers := headerRecordingServer(t)
	body := "mcp_servers:\n  httpsrv:\n    url: " + remote.URL + "\n    headers:\n      Authorization: 'Bearer ${" + scopeTokenName + "}'\n" +
		"  stdiosrv:\n    command: " + mockBinary(t) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := mcpRegistryScope(ctx)
	t.Cleanup(func() { ShutdownMCPServers(context.Background(), &scope, nil, 5*time.Second) })
	if currentSecretScopeHome(ctx) != "" {
		t.Fatal("discovery starts unscoped")
	}
	if _, err := ConnectAll(ctx, env, nil); err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	_, httpLive := mcpServers[ServerKey{Scope: scope, Name: "httpsrv"}]
	_, stdioLive := mcpServers[ServerKey{Scope: scope, Name: "stdiosrv"}]
	mcpMu.Unlock()
	if !httpLive || !stdioLive {
		t.Fatalf("discover handed over http %v stdio %v", httpLive, stdioLive)
	}
	for _, h := range headers() {
		if h != "Bearer "+scopeTokenValue {
			t.Fatalf("header %q, want the owning profile's value", h)
		}
	}
	if len(headers()) == 0 || currentSecretScopeHome(ctx) != "" {
		t.Fatal("the remote server saw no request or the caller stayed scoped")
	}
}

func TestConnectFailureAfterClaimLeavesTheClaimWithDiscovery(t *testing.T) {
	_, env := managerHome(t)
	dir := filepath.Join(t.TempDir(), "unhydrated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	multiplexOn(t)
	ctx := home.WithOverride(context.Background(), dir)
	var claimed *serverTask
	srv, err := connectServer(ctx, env, "demo", &Rendered{Config: remoteEntry("https://example.invalid/mcp")}, func(s *serverTask) { claimed = s })
	if err == nil || srv != nil {
		t.Fatalf("an unresolved owner credential must fail the connect, got %v", err)
	}
	if claimed == nil || claimed.shutdownEv.IsSet() {
		t.Fatal("the claimed task stays with discovery, unreaped by the connect")
	}
	claimed.shutdown(context.Background())
	if currentSecretScopeHome(ctx) != "" {
		t.Fatal("the caller's context gained a scope binding")
	}
}

// Connects demo under a boot time scope snapshot of h, returning the connect error
func connectUnderBootScope(t *testing.T, env *tools.Env, h, url string) error {
	ctx := home.WithOverride(context.Background(), h)
	ctx = withSecretScope(ctx, profileSecretMapping(h), h)
	srv, err := connectServer(ctx, env, "demo", &Rendered{Config: remoteEntry(url)}, nil)
	if err == nil {
		srv.shutdown(context.Background())
	}
	return err
}

func TestConnectRendersRemoteHeadersUnderTheOwnersFreshScope(t *testing.T) {
	_, env := managerHome(t)
	tagSecretSource(t)
	t.Setenv(scopeTokenName, "launch-env-value")
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "s6probe-a"), filepath.Join(root, "s6probe-b")
	for _, d := range []string{homeA, homeB} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeEnvFile(t, homeA, scopeTokenName+"=value-a\n")
	multiplexOn(t)
	remote, headers := headerRecordingServer(t)
	last := func() string { h := headers(); return h[len(h)-1] }
	if err := connectUnderBootScope(t, env, homeA, remote.URL); err != nil || last() != "Bearer value-a" {
		t.Fatalf("A connect %v %q", err, headers())
	}
	err := connectUnderBootScope(t, env, homeB, remote.URL)
	if err == nil || !strings.Contains(err.Error(), "'demo'") || !strings.Contains(err.Error(), scopeTokenName) {
		t.Fatalf("B before its source answered must fail closed, got %v", err)
	}
	writeEnvFile(t, homeB, scopeTokenName+"=value-b\n")
	if err := connectUnderBootScope(t, env, homeB, remote.URL); err != nil || last() != "Bearer value-b" {
		t.Fatalf("B connect %v %q", err, headers())
	}
	if err := connectUnderBootScope(t, env, homeA, remote.URL); err != nil || last() != "Bearer value-a" {
		t.Fatalf("A again %v %q", err, headers())
	}
	for _, h := range headers() {
		if strings.Contains(h, "launch-env-value") || strings.Contains(h, "${") {
			t.Fatalf("header %q left the owner scope", h)
		}
	}
}

func TestLaunchProfileEnvOnlyCredentialSurvivesTheOwnerRebuild(t *testing.T) {
	if os.Getenv(freshLaunchEnvChild) == "" {
		runInFreshLaunchEnvChild(t, scopeTokenName+"=tok-from-systemd")
		return
	}
	launchHome, env := managerHome(t)
	writeEnvFile(t, launchHome, "")
	provider.ActivateMultiProfileHosting()
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	remote, headers := headerRecordingServer(t)
	ctx := provider.WithLaunchProfileRuntimeScope(context.Background(), launchHome)
	srv, err := connectServer(ctx, env, "demo", &Rendered{Config: remoteEntry(remote.URL)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.shutdown(context.Background())
	rebuilt, err := interpolateEnvVars(installOwnerSecretScope(ctx), ownerEnv(ctx, env), map[string]any{"Authorization": "Bearer ${" + scopeTokenName + "}"})
	if err != nil {
		t.Fatal(err)
	}
	if h := headers(); len(h) == 0 || h[0] != "Bearer tok-from-systemd" || rebuilt.(map[string]any)["Authorization"] != "Bearer tok-from-systemd" {
		t.Fatalf("connect %q rebuild %v", h, rebuilt)
	}
}

// Reruns the calling test alone in a child process whose launch env snapshot is still uncaptured
func runInFreshLaunchEnvChild(t *testing.T, extraEnv ...string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.count=1", "-test.v")
	cmd.Env = append(append(os.Environ(), freshLaunchEnvChild+"="+t.Name()), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("fresh launch env child %v\n%s", err, out)
	}
}

func TestReconnectRerendersRemoteHeadersUnderTheOwnersFreshScope(t *testing.T) {
	_, env := managerHome(t)
	tagSecretSource(t)
	homeB := filepath.Join(t.TempDir(), "s6probe-b")
	if err := os.MkdirAll(homeB, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "mcp_servers:\n  demo:\n    url: https://example.invalid/mcp\n    headers:\n      Authorization: 'Bearer ${" + scopeTokenName + "}'\n"
	if err := os.WriteFile(filepath.Join(homeB, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	multiplexOn(t)
	ctx := home.WithOverride(context.Background(), homeB)
	ctx = withSecretScope(ctx, profileSecretMapping(homeB), homeB)
	srv := newServerTask("demo", ownerEnv(ctx, env))
	var seen []string
	srv.transportRunner = func(_ context.Context, r *Rendered) (string, error) {
		seen = append(seen, r.Config["headers"].(map[string]any)["Authorization"].(string))
		srv.mu.Lock()
		srv.sessionProven = true
		srv.mu.Unlock()
		srv.ready.Set()
		if len(seen) == 1 {
			writeEnvFile(t, homeB, scopeTokenName+"=value-b\n")
			return "reconnect", nil
		}
		srv.shutdownEv.Set()
		return "shutdown", nil
	}
	done := make(chan struct{})
	go func() { defer close(done); srv.run(ctx, &Rendered{Config: remoteEntry("https://example.invalid/mcp")}) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not finish")
	}
	if len(seen) != 2 || seen[0] != "Bearer ${"+scopeTokenName+"}" || seen[1] != "Bearer value-b" {
		t.Fatalf("headers %q", seen)
	}
}
