package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Account source with a guest identity, so connectors are available and no guest is ever minted
type connectorsAccount struct{ NousAccountSource }

func (connectorsAccount) IdentityState(*Env) (bool, bool)          { return true, true }
func (connectorsAccount) GuestAllowed(*Env) bool                   { return false }
func (connectorsAccount) PeekAccessToken(*Env) string              { return "" }
func (connectorsAccount) ReadAccessToken(*Env) string              { return "" }
func (connectorsAccount) LastMintFailure(*Env) (MintFailure, bool) { return MintFailure{}, false }
func (connectorsAccount) ManagedToolsRolledOut(*Env) bool          { return true }

// Opens or closes the connectors availability gate for one test with fresh check and definition caches
func connectorsGate(t *testing.T, open bool) {
	t.Helper()
	old := NousAccount
	NousAccount = nil
	if open {
		NousAccount = connectorsAccount{}
	}
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() {
		NousAccount = old
		InvalidateCheckCache()
		ClearToolDefinitionsCache()
	})
}

// Tool names a session receives through the real assembly with the tool_search step skipped
func sessionToolNames(t *testing.T, enabled, disabled []string, connectors bool) map[string]bool {
	t.Helper()
	connectorsGate(t, connectors)
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	names := map[string]bool{}
	for _, td := range Default.computeToolDefinitions(context.Background(), env, DefinitionOptions{Enabled: enabled, Disabled: disabled, Quiet: true, SkipToolSearchAssembly: true}) {
		names[toolDefName(td)] = true
	}
	return names
}

// Sorted platform toolsets of a config read under a temp home, the include_default_mcp_servers runtime variant
func cliPlatformToolsets(t *testing.T, platform string, cfg config.Tree, includeMCP bool) []string {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	set := Default.PlatformTools(context.Background(), "", cfg, platform, PlatformToolsOptions{IncludeDefaultMCPServers: includeMCP})
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func TestConnectorsToolCLISessionGetsTheToolOutsideACodeWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	enabled := cliPlatformToolsets(t, "cli", config.Tree{}, true)
	if !slices.Contains(enabled, "connections") || !sessionToolNames(t, enabled, nil, true)["manage_connections"] {
		t.Fatalf("enabled %v", enabled)
	}
}

func TestConnectorsToolFocusModeCodingPostureGetsTheTool(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	env := &Env{Home: t.TempDir(), Config: config.Tree{"agent": map[string]any{"coding_context": "focus"}}}
	selection := CodingSelection(env, "cli", repo)
	if !reflect.DeepEqual(selection, []string{"coding"}) || !sessionToolNames(t, selection, nil, true)["manage_connections"] {
		t.Fatalf("selection %v", selection)
	}
}

func TestConnectorsToolSessionThePortalHasNotEnabledNeverReceivesTheTool(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, selection := range [][]string{cliPlatformToolsets(t, "cli", config.Tree{}, true), {"coding"}} {
		if sessionToolNames(t, selection, nil, false)["manage_connections"] {
			t.Fatalf("selection %v carries the tool", selection)
		}
	}
	connectorsGate(t, false)
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	out := NewDispatcher(Default).RegistryDispatch(context.Background(), &Call{Name: "manage_connections", Args: map[string]any{"action": "status"}, Env: env})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || !strings.Contains(asStr(parsed["error"]), "not available in this session") {
		t.Fatalf("out %s", out)
	}
}

func TestConnectorsToolOperatorCanStillTurnItOff(t *testing.T) {
	t.Chdir(t.TempDir())
	enabled := cliPlatformToolsets(t, "cli", config.Tree{}, true)
	if sessionToolNames(t, enabled, []string{"connections"}, true)["manage_connections"] {
		t.Fatal("disabled connections kept the tool")
	}
	if !sessionToolNames(t, enabled, []string{"icebath-cli"}, true)["manage_connections"] {
		t.Fatal("a disabled platform composite stripped the tool")
	}
}

func TestConnectorsToolIsNeverDeferrable(t *testing.T) {
	if IsDeferrableToolName(context.Background(), &Env{Home: t.TempDir()}, "manage_connections", nil) {
		t.Fatal("manage_connections is deferrable")
	}
}

func TestOrdinaryPlatformDefaultsGrantConnectionsWithoutWideningWebhook(t *testing.T) {
	for _, platform := range []string{"cli", "telegram"} {
		enabled := cliPlatformToolsets(t, platform, config.Tree{}, false)
		resolved := map[string]bool{}
		for _, ts := range enabled {
			for _, name := range Default.ResolveToolset(context.Background(), "", ts) {
				resolved[name] = true
			}
		}
		if !slices.Contains(enabled, "connections") || !resolved["manage_connections"] {
			t.Fatalf("%s enabled %v", platform, enabled)
		}
	}
	if slices.Contains(cliPlatformToolsets(t, "webhook", config.Tree{}, false), "connections") {
		t.Fatal("webhook widened to connections")
	}
	for _, selection := range [][]any{{}, {"safe"}, {"file"}} {
		cfg := config.Tree{"platform_toolsets": map[string]any{"cli": selection}}
		if slices.Contains(cliPlatformToolsets(t, "cli", cfg, false), "connections") {
			t.Fatalf("selection %v widened to connections", selection)
		}
	}
}

// Gateway at CONNECTOR_GATEWAY_URL and the Portal answering every connector route, recording each remote leg
func connectorTestGateway(t *testing.T, execute func(body map[string]any) map[string]any) (*sync.Mutex, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var remote []string
	schema := map[string]any{"GMAIL_SEND_EMAIL": map[string]any{"connector": "gmail", "tool": "SEND_EMAIL", "description": "Send mail", "inputSchema": map[string]any{}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		var reply map[string]any
		leg := ""
		switch {
		case r.URL.Path == "/api/v1/connectors/accounts":
			reply = map[string]any{"accounts": []any{}}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/search"):
			leg, reply = "search", map[string]any{"results": []any{map[string]any{"index": 0, "tools": []any{"GMAIL_SEND_EMAIL"}}}, "schemas": schema}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/schemas"):
			leg, reply = "describe", map[string]any{"schemas": schema}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/execute"):
			leg, reply = "execute", execute(body)
		case strings.HasSuffix(r.URL.Path, "/v1/connectors") && r.Method == http.MethodGet:
			leg, reply = "status", map[string]any{"items": []any{}}
		default:
			http.NotFound(w, r)
			return
		}
		if leg != "" {
			mu.Lock()
			remote = append(remote, leg)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "t")
	oldPortal := NousPortalBaseURL
	NousPortalBaseURL = func(context.Context) (string, error) { return srv.URL, nil }
	t.Cleanup(func() { NousPortalBaseURL = oldPortal })
	return &mu, &remote
}

// One model tool call through the dispatcher with the session's toolset scope, decoded
func scopedCall(t *testing.T, env *Env, name string, args map[string]any, opts DispatchOptions) map[string]any {
	t.Helper()
	out := DispatcherOf(context.Background()).DispatchWith(context.Background(), &Call{Name: name, Args: args, SessionID: "s", Env: env}, opts)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("%s: %v in %s", name, err, out)
	}
	return parsed
}

// Python membership of name in a list of strings or the keys of a mapping
func pyIn(name string, container any) bool {
	switch c := container.(type) {
	case []any:
		return slices.Contains(c, any(name))
	case map[string]any:
		_, ok := c[name]
		return ok
	}
	return false
}

func TestConnectorAccountACallRanUnderReachesTheModelOnBothDispatchPaths(t *testing.T) {
	connectorsGate(t, true)
	connectorTestGateway(t, func(body map[string]any) map[string]any {
		if call, ok := body["call"].(map[string]any); ok {
			args, _ := call["arguments"].(map[string]any)
			return map[string]any{"result": map[string]any{"connector": call["connector"], "tool": call["tool"], "data": map[string]any{"ok": true}, "account": args["connector_alias"]}}
		}
		var results []any
		for i, raw := range body["tools"].([]any) {
			c := raw.(map[string]any)
			args, _ := c["arguments"].(map[string]any)
			results = append(results, map[string]any{"index": i, "connector": c["connector"], "tool": c["tool"], "data": map[string]any{"ok": true}, "account": args["connector_alias"]})
		}
		return map[string]any{"results": results, "successCount": len(results), "errorCount": 0, "totalCount": len(results)}
	})
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	opts := DispatchOptions{EnabledToolsets: []string{"connections"}, SkipPreToolCallHook: true, SkipToolRequestMiddleware: true, SkipToolExecutionMiddleware: true}
	if single := scopedCall(t, env, "connectors__gmail__FETCH_EMAILS", map[string]any{"connector_alias": "work"}, opts); single["account"] != "work" {
		t.Fatalf("single %v", single)
	}
	batch := scopedCall(t, env, "tool_call", map[string]any{"calls": []any{
		map[string]any{"name": "connectors__gmail__FETCH_EMAILS", "arguments": map[string]any{"connector_alias": "home"}},
		map[string]any{"name": "connectors__gmail__FETCH_EMAILS", "arguments": map[string]any{}},
	}}, opts)
	results, _ := batch["results"].([]any)
	var accounts []any
	for _, raw := range results {
		entry, _ := raw.(map[string]any)
		accounts = append(accounts, entry["account"])
	}
	if !reflect.DeepEqual(accounts, []any{"home", nil}) {
		t.Fatalf("batch %v", batch)
	}
}

func TestConnectorScopeControlsSchemaDiscoveryAndExecution(t *testing.T) {
	name := "connectors__gmail__SEND_EMAIL"
	for _, tc := range []struct {
		enabled, disabled []string
		allowed           bool
	}{
		{[]string{}, []string{}, false}, {[]string{"safe"}, []string{}, false}, {[]string{"icebath-webhook"}, []string{}, false},
		{[]string{"connections"}, []string{}, true}, {[]string{"icebath-cli"}, []string{"connections"}, false},
		{[]string{"safe"}, []string{"connections"}, false}, {nil, []string{"connections"}, false}, {nil, []string{}, true},
	} {
		t.Run(strings.Join(tc.enabled, ",")+"-"+strings.Join(tc.disabled, ","), func(t *testing.T) {
			connectorsGate(t, true)
			mu, remote := connectorTestGateway(t, func(map[string]any) map[string]any {
				return map[string]any{"result": map[string]any{"connector": "gmail", "tool": "SEND_EMAIL", "data": "sent"}}
			})
			env := &Env{Home: t.TempDir()}
			t.Setenv("ICEBATH_HOME", env.Home)
			ctx := context.Background()
			defs := Default.ToolDefinitions(ctx, env, DefinitionOptions{Enabled: tc.enabled, Disabled: tc.disabled, Quiet: true, SkipToolSearchAssembly: true})
			has := false
			for _, td := range defs {
				has = has || toolDefName(td) == "manage_connections"
			}
			if has != tc.allowed {
				t.Fatalf("manage_connections in schema %v", has)
			}
			if tc.enabled != nil && len(tc.enabled) == 0 {
				if got := Default.ToolDefinitions(ctx, env, DefinitionOptions{Enabled: tc.enabled, Disabled: tc.disabled, Quiet: true}); len(got) != 0 {
					t.Fatalf("empty selection defs %v", got)
				}
			}
			opts := DispatchOptions{EnabledToolsets: tc.enabled, DisabledToolsets: tc.disabled}
			search := scopedCall(t, env, "tool_search", map[string]any{"queries": []any{"send mail"}}, opts)
			if got := pyIn(name, search["tools"]); got != tc.allowed {
				t.Fatalf("tool_search lists the connector %v in %v", got, search)
			}
			if got := pyIn(name, scopedCall(t, env, "tool_describe", map[string]any{"names": []any{name}}, opts)["tools"]); got != tc.allowed {
				t.Fatalf("tool_describe lists the connector %v", got)
			}
			result := scopedCall(t, env, "tool_call", map[string]any{"calls": []any{map[string]any{"name": name, "arguments": map[string]any{}}}}, opts)
			direct := scopedCall(t, env, name, map[string]any{}, opts)
			status := scopedCall(t, env, "manage_connections", map[string]any{"action": "status"}, opts)
			mu.Lock()
			legs := append([]string(nil), *remote...)
			mu.Unlock()
			if tc.allowed {
				first, _ := result["results"].([]any)
				row, _ := first[0].(map[string]any)
				if row["response"] != "sent" || status["error"] != nil || direct["response"] != "sent" {
					t.Fatalf("result %v status %v direct %v", result, status, direct)
				}
				if !reflect.DeepEqual(legs, []string{"search", "describe", "execute", "execute", "status"}) {
					t.Fatalf("remote %v", legs)
				}
				return
			}
			for _, out := range []map[string]any{result, status, direct} {
				if raw, _ := json.Marshal(out); !strings.Contains(string(raw), "not available in this session") {
					t.Fatalf("out %s", raw)
				}
			}
			if len(legs) != 0 {
				t.Fatalf("remote %v", legs)
			}
		})
	}
}
