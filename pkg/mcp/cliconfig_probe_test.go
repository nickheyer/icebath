package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

type probeSeen struct {
	mu        sync.Mutex
	outer     time.Duration
	inner     time.Duration
	config    map[string]any
	servers   []*serverTask
	connectFn func(ctx context.Context, env *tools.Env, name string, r *Rendered, claim func(*serverTask)) (*serverTask, error)
}

func observeProbe(t *testing.T, connect bool) *probeSeen {
	t.Helper()
	seen := &probeSeen{}
	prevConnect, prevLoop := probeConnect, probeRunOnLoop
	probeRunOnLoop = func(ctx context.Context, timeout time.Duration, fn func(ctx context.Context) ([]ToolSummary, error)) ([]ToolSummary, error) {
		seen.mu.Lock()
		seen.outer = timeout
		seen.mu.Unlock()
		return prevLoop(ctx, timeout, fn)
	}
	probeConnect = func(ctx context.Context, env *tools.Env, name string, r *Rendered, claim func(*serverTask)) (*serverTask, error) {
		dl, _ := ctx.Deadline()
		seen.mu.Lock()
		seen.inner, seen.config = time.Until(dl), r.Config
		seen.mu.Unlock()
		if !connect {
			return nil, errors.New("connect intercepted")
		}
		srv, err := prevConnect(ctx, env, name, r, claim)
		seen.mu.Lock()
		seen.servers = append(seen.servers, srv)
		seen.mu.Unlock()
		return srv, err
	}
	t.Cleanup(func() { probeConnect, probeRunOnLoop = prevConnect, prevLoop })
	return seen
}

func TestProbeEnvResolution(t *testing.T) {
	h := isolateHome(t)
	ctx := context.Background()
	t.Setenv("MCP_N8N_API_KEY", "jwt-token-xyz")
	r, err := renderServerConfig(ctx, &tools.Env{}, map[string]any{"url": "http://localhost:5678/mcp-server/http",
		"headers": map[string]any{"Authorization": "Bearer ${MCP_N8N_API_KEY}"}})
	if err != nil || r.Config["headers"].(map[string]any)["Authorization"] != "Bearer jwt-token-xyz" {
		t.Fatalf("rendered %v %v", r, err)
	}
	t.Setenv("MCP_SHARED_API_KEY", "default-secret")
	if err := os.WriteFile(filepath.Join(h, ".env"), []byte("MCP_SHARED_API_KEY=profile-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = renderServerConfig(ctx, &tools.Env{Secrets: config.NewSecrets(h)}, map[string]any{"headers": map[string]any{"Authorization": "Bearer ${MCP_SHARED_API_KEY}"}})
	if err != nil || r.Config["headers"].(map[string]any)["Authorization"] != "Bearer profile-secret" || os.Getenv("MCP_SHARED_API_KEY") != "default-secret" {
		t.Fatalf("scoped %v %v env %q", r, err, os.Getenv("MCP_SHARED_API_KEY"))
	}
	var auth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		auth = req.Header.Get("Authorization")
		mu.Unlock()
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(body, &msg)
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": LatestHandshakeVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "n8n", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "do_thing", "description": "a tool", "inputSchema": map[string]any{"type": "object"}}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	found, err := probeSingleServer(ctx, &tools.Env{}, "n8n", map[string]any{"url": srv.URL + "/mcp-server/http",
		"headers": map[string]any{"Authorization": "Bearer ${MCP_N8N_API_KEY}"}})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || !reflect.DeepEqual(found, []ToolSummary{{Name: "do_thing", Description: "a tool"}}) || auth != "Bearer jwt-token-xyz" {
		t.Fatalf("found %v err %v auth %q", found, err, auth)
	}
	seen := observeProbe(t, false)
	probeServer(ctx, &tools.Env{}, "travelermd", map[string]any{"url": "https://mcp.traveler.md/mcp", "auth": "oauth"}, fptr(315), nil)
	if seen.config["connect_timeout"] != 315.0 || seen.outer != 325*time.Second {
		t.Fatalf("config %v outer %v", seen.config, seen.outer)
	}
}

func TestMCPCmdRemoveEvictsInMemoryProvider(t *testing.T) {
	seedServers(t, map[string]any{"oauth-srv": map[string]any{"url": "https://example.com/mcp", "auth": "oauth"}})
	ctx := context.Background()
	m := getManager()
	if _, err := m.getOrBuildProvider(ForceInteractiveOAuth(ctx), "oauth-srv", "https://example.com/mcp", nil, HTTPOptions{}); err != nil {
		t.Fatal(err)
	}
	has := func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		_, ok := m.entries[m.key(ctx, "oauth-srv", "")]
		return ok
	}
	if !has() {
		t.Fatal("provider was not cached")
	}
	ui := &cliUI{inputs: []string{"y"}}
	ui.interacted = true
	cmdMCPRemove(ctx, ui, "oauth-srv")
	if has() {
		t.Fatalf("provider still cached after remove: %s", ui.out.String())
	}
}

func TestMCPCmdLoginKeepsDiscoveredServerMetadata(t *testing.T) {
	h := seedServers(t, map[string]any{"tv": map[string]any{"url": "https://mcp.example.com/mcp", "auth": "oauth"}})
	dir := filepath.Join(h, "mcp-tokens")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "tv.json"), []byte(`{"access_token": "stale"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "tv.client.json"), []byte(`{"client_id": "old"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "tv.meta.json"), []byte(`{"issuer": "https://www.example.com", "authorization_endpoint": "https://www.example.com/oauth/authorize", "token_endpoint": "https://www.example.com/oauth/token"}`), 0o600)
	atProbe := map[string]bool{}
	prev := cliProbe
	cliProbe = func(ctx context.Context, env *tools.Env, name string, cfg map[string]any, ct *float64, d *ProbeDetails) ([]ToolSummary, error) {
		matches, _ := filepath.Glob(filepath.Join(dir, "tv*"))
		for _, p := range matches {
			atProbe[filepath.Base(p)] = true
		}
		os.WriteFile(filepath.Join(dir, "tv.json"), []byte(`{"access_token": "fresh", "token_type": "Bearer"}`), 0o600)
		return []ToolSummary{{"a", "d"}}, nil
	}
	t.Cleanup(func() { cliProbe = prev })
	ui := &cliUI{}
	cmdMCPLogin(context.Background(), &tools.Env{}, ui, "tv", "")
	if !atProbe["tv.meta.json"] || atProbe["tv.json"] || atProbe["tv.client.json"] {
		t.Fatalf("token dir at probe %v", atProbe)
	}
	if !strings.Contains(ui.out.String(), "Authenticated — 1 tool(s) available") {
		t.Fatal(ui.out.String())
	}
}

func TestMCPCmdReauthAllReportsPartialFailures(t *testing.T) {
	h := seedServers(t, map[string]any{"a": map[string]any{"url": "https://a.example.com/mcp", "auth": "oauth"},
		"b": map[string]any{"url": "https://b.example.com/mcp", "auth": "oauth"}})
	prev := cliProbe
	cliProbe = func(ctx context.Context, env *tools.Env, name string, cfg map[string]any, ct *float64, d *ProbeDetails) ([]ToolSummary, error) {
		if name != "a" {
			return nil, errors.New("authorization denied")
		}
		os.MkdirAll(filepath.Join(h, "mcp-tokens"), 0o700)
		os.WriteFile(filepath.Join(h, "mcp-tokens", "a.json"), []byte(`{"access_token": "x", "token_type": "Bearer"}`), 0o600)
		return []ToolSummary{{"t", "d"}}, nil
	}
	t.Cleanup(func() { cliProbe = prev })
	ui := &cliUI{}
	cmdMCPReauth(context.Background(), &tools.Env{}, ui, "", true)
	if !strings.Contains(ui.out.String(), "Re-authenticated 1/2 server(s)") {
		t.Fatal(ui.out.String())
	}
}

func TestMCPCmdDispatcherNoActionShowsList(t *testing.T) {
	seedServers(t, map[string]any{})
	ui := &cliUI{}
	if _, err := MCPCommand(context.Background(), &tools.Env{}, ui, MCPArgs{}); err != nil {
		t.Fatal(err)
	}
	if out := ui.out.String(); !strings.Contains(out, "Commands:") && !strings.Contains(out, "No MCP servers") {
		t.Fatal(out)
	}
}

func TestMCPCmdTestSuccessDisplayMasksHeaders(t *testing.T) {
	for _, c := range []struct{ header, want string }{
		{"Bearer " + synthetic, "Authorization:"},
		{"Bearer ${MCP_TEST_TOKEN}; backup=" + synthetic, "***"},
	} {
		seedServers(t, map[string]any{"ink": map[string]any{"url": "https://mcp.example/mcp", "headers": map[string]any{"Authorization": c.header}}})
		setCLIProbe(t, []ToolSummary{{"ping", "Ping"}}, nil)
		ui := &cliUI{}
		cmdMCPTest(context.Background(), &tools.Env{}, ui, "ink")
		out := ui.out.String()
		assertFullyRedacted(t, out)
		if !strings.Contains(out, c.want) || !strings.Contains(out, "Connected") {
			t.Fatalf("header %q display %s", c.header, out)
		}
	}
}

func TestMCPCmdLoginRedactsRawProbeException(t *testing.T) {
	seedServers(t, map[string]any{"ink": map[string]any{"url": "https://mcp.example/mcp", "auth": "oauth"}})
	setCLIProbe(t, nil, errors.New("connect failed: Authorization: Bearer "+synthetic))
	ui := &cliUI{}
	cmdMCPLogin(context.Background(), &tools.Env{}, ui, "ink", "")
	out := ui.out.String()
	assertFullyRedacted(t, out)
	if !strings.Contains(out, "Authentication failed") || !strings.Contains(out, "Bearer ***") {
		t.Fatal(out)
	}
}

func seedDeviceServer(t *testing.T, mode string) (string, string, func() []deviceWireRow) {
	t.Helper()
	dir := freshManager(t)
	fakeDeviceClock(t)
	testStderr.Reset()
	base, wire := deviceFixture(t, mode)
	if err := config.SaveUser(context.Background(), dir, map[string]any{"mcp_servers": map[string]any{"fixture": map[string]any{"url": base + "/mcp", "auth": "oauth",
		"oauth": deviceOAuthConfig(mode)}}}); err != nil {
		t.Fatal(err)
	}
	return dir, base, wire
}

func runDeviceLogin(mode string) string {
	ui := &cliUI{}
	if mode == "preregistered" {
		cmdMCPReauth(context.Background(), &tools.Env{}, ui, "fixture", false)
	} else {
		cmdMCPLogin(context.Background(), &tools.Env{}, ui, "fixture", "device")
	}
	return ui.out.String() + testStderr.String()
}

func TestMCPCmdDeviceLoginCLIOutput(t *testing.T) {
	for _, mode := range []string{"success", "preregistered", "multi_issuer"} {
		t.Run(mode, func(t *testing.T) {
			dir, _, wire := seedDeviceServer(t, mode)
			out := runDeviceLogin(mode)
			if !strings.Contains(out, "TEST-CODE") || !strings.Contains(out, "Authenticated") {
				t.Fatalf("login output %s", out)
			}
			tokenPath := filepath.Join(dir, "mcp-tokens", "fixture.json")
			tokens := readJSONFile(t, tokenPath)
			tokens["expires_at"] = float64(time.Now().Unix() - 60)
			writeTokenFile(t, dir, "fixture.json", tokens)
			ui := &cliUI{}
			cmdMCPTest(context.Background(), &tools.Env{}, ui, "fixture")
			if !strings.Contains(ui.out.String(), "Connected") {
				t.Fatalf("refresh output %s", ui.out.String())
			}
			refreshed := false
			for _, row := range wire() {
				if row.path == "/token" && row.data["grant_type"] == "refresh_token" {
					refreshed = true
				}
				if row.path == "/register" && mode == "preregistered" {
					t.Fatal("preregistered client registered")
				}
			}
			if !refreshed {
				t.Fatal("no refresh grant behind the test command")
			}
		})
	}
}

func TestMCPCmdDeviceLoginFailureCLIOutput(t *testing.T) {
	for _, mode := range []string{"denied", "expiry", "unsupported", "issuer", "resource", "malformed", "persistence"} {
		t.Run(mode, func(t *testing.T) {
			dir, _, _ := seedDeviceServer(t, mode)
			previous := map[string]string{}
			if mode == "persistence" {
				previous = map[string]string{"fixture.json": `{"access_token":"old-fixture","token_type":"Bearer"}`,
					"fixture.client.json": `{"client_id":"old-client"}`, "fixture.meta.json": `{"issuer":"https://old.example"}`}
				for name, v := range previous {
					writeTokenFile(t, dir, name, v)
				}
				deviceSetTokens = func(*TokenStorage, context.Context, *OAuthToken) error { return errors.New("fixture disk failure") }
				t.Cleanup(func() { deviceSetTokens = (*TokenStorage).SetTokens })
			}
			out := runDeviceLogin(mode)
			if !strings.Contains(out, "Authentication failed") || strings.Contains(out, "fixture-device-secret") || strings.Contains(out, "Authenticated") {
				t.Fatalf("output %s", out)
			}
			for name, v := range previous {
				if raw, _ := os.ReadFile(filepath.Join(dir, "mcp-tokens", name)); string(raw) != v {
					t.Fatalf("%s not preserved %q", name, raw)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "mcp-tokens", "fixture.json")); (err == nil) != (mode == "persistence") {
				t.Fatalf("token persisted %v", err)
			}
		})
	}
}
