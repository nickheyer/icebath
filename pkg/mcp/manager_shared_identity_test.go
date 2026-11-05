package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Default and worker profile homes under a temp HOME, the default being the process home
func defaultAndWorkerHomes(t *testing.T) map[string]string {
	t.Helper()
	managerHome(t)
	root := t.TempDir()
	t.Setenv("HOME", root)
	def := home.PlatformDefault()
	worker := filepath.Join(def, home.ProfilesDir, "worker")
	if err := os.MkdirAll(worker, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", def)
	t.Setenv("NO_PROXY", "*")
	return map[string]string{"default": def, "worker": worker}
}

// Profile runtime scope of the gateway, the profile home with its own secret mapping bound
func profileRuntimeScope(h string) (context.Context, *tools.Env) {
	ctx := withSecretScope(home.WithOverride(context.Background(), h), profileSecretMapping(h), h)
	return ctx, &tools.Env{Home: h, Secrets: config.NewSecrets(h)}
}

// Runs startup discovery once per profile under its runtime scope, then calls tool in each profile
func discoverAndCall(t *testing.T, homes map[string]string, tool string, args map[string]any) map[string]string {
	t.Helper()
	names := []string{"default", "worker"}
	for _, name := range names {
		ctx, env := profileRuntimeScope(homes[name])
		if _, err := ConnectAll(ctx, env, nil); err != nil {
			t.Fatalf("discovery for %s: %v", name, err)
		}
	}
	results := map[string]string{}
	for _, name := range names {
		ctx, env := profileRuntimeScope(homes[name])
		out := tools.NewDispatcher(tools.Default).RegistryDispatch(ctx, &tools.Call{Name: tool, Args: args, Env: env})
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("%s result %q", name, out)
		}
		result, ok := decoded["result"].(string)
		if !ok {
			t.Fatalf("%s result %q", name, out)
		}
		results[name] = result
	}
	return results
}

func TestSharedConnectionProfileWithOtherSecretSourceValueGetsItsOwnStdioConnection(t *testing.T) {
	homes := defaultAndWorkerHomes(t)
	prev := secretSourceNamesFn
	secretSourceNamesFn = func() []string { return []string{"GH_TOKEN"} }
	t.Cleanup(func() { secretSourceNamesFn = prev })
	for name, h := range homes {
		writeEnvFile(t, h, "GH_TOKEN=fake-token-"+name+"\n")
		writeProfileConfig(t, h, fmt.Sprintf("mcp_servers:\n  gh:\n    command: %q\n    args: []\n    env:\n      MOCK_MCP_TOOLS: env\n", mockBinary(t)))
	}
	results := discoverAndCall(t, homes, "mcp__gh__env", map[string]any{})
	got := map[string]string{}
	for name, raw := range results {
		var childEnv map[string]string
		if err := json.Unmarshal([]byte(raw), &childEnv); err != nil {
			t.Fatalf("%s child env %q", name, raw)
		}
		got[name] = "GH_TOKEN=" + childEnv["GH_TOKEN"]
	}
	if want := map[string]string{"default": "GH_TOKEN=fake-token-default", "worker": "GH_TOKEN=fake-token-worker"}; !maps.Equal(got, want) {
		t.Fatalf("child tokens %v", got)
	}
}

func TestSharedConnectionProfileWithOtherProfileIdentityHeaderGetsItsOwnHTTPConnection(t *testing.T) {
	homes := defaultAndWorkerHomes(t)
	var mu sync.Mutex
	var calls [][2]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &msg)
		mu.Lock()
		calls = append(calls, [2]string{msg.Method, r.Header.Get("X-Icebath-Profile")})
		mu.Unlock()
		if len(msg.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": LatestHandshakeVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "team", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "save_note", "inputSchema": map[string]any{"type": "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "saved"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	for _, h := range homes {
		writeProfileConfig(t, h, fmt.Sprintf("mcp_servers:\n  team:\n    url: %s/mcp\n    identity_header:\n      name: X-Icebath-Profile\n      value_from: profile\n", srv.URL))
	}
	discoverAndCall(t, homes, "mcp__team__save_note", map[string]any{"text": "hi"})
	mu.Lock()
	defer mu.Unlock()
	var profiles []string
	for _, c := range calls {
		if c[0] == "tools/call" {
			profiles = append(profiles, c[1])
		}
	}
	if !slices.Equal(profiles, []string{"default", "worker"}) {
		t.Fatalf("tools/call identities %v", profiles)
	}
}
