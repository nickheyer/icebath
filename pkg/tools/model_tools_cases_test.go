package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// A legacy subset entry is reported, cleared by enabling its toolset, and the tools come back
func TestLegacySearchEntryIsReportedThenClearedByEnablingWeb(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("EXA_API_KEY", "exa-test")
	InvalidateCheckCache()
	ClearToolDefinitionsCache()
	t.Cleanup(func() { InvalidateCheckCache(); ClearToolDefinitionsCache() })
	ctx := context.Background()
	cfg := config.Tree{"platform_toolsets": map[string]any{"cli": []any{"file", "terminal", "web"}},
		"agent": map[string]any{"disabled_toolsets": []any{"search", "terminal_tools", "browser", "memory"}}}
	enabled := platformTools(cfg, "cli", false)
	want := map[string]PartialStrip{
		"terminal": {Stripped: []string{"terminal"}, Entries: []string{"terminal_tools"}, Hint: "run `icebath tools enable terminal` to restore"},
		"web":      {Stripped: []string{"web_search"}, Entries: []string{"search"}, Hint: "run `icebath tools enable web` to restore"},
	}
	if got := Default.ToolsetsPartiallyStripped(ctx, "", cfg, enabled); !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped %+v", got)
	}
	if err := Default.SavePlatformTools(ctx, "", cfg, "cli", enabled, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	disabled := ParseConfigStringList(cfg.Get("agent.disabled_toolsets"))
	if !reflect.DeepEqual(disabled, []string{"browser", "memory"}) {
		t.Fatalf("disabled %v", disabled)
	}
	enabled = platformTools(cfg, "cli", false)
	if got := Default.ToolsetsPartiallyStripped(ctx, "", cfg, enabled); len(got) != 0 {
		t.Fatalf("still stripped %+v", got)
	}
	names := map[string]bool{}
	for _, td := range Default.ToolDefinitions(ctx, &Env{Home: t.TempDir()}, DefinitionOptions{Enabled: sortedSet(enabled), Disabled: disabled, Quiet: true}) {
		names[toolDefName(td)] = true
	}
	for _, n := range []string{"web_search", "web_extract", "terminal"} {
		if !names[n] {
			t.Fatalf("%s missing from %v", n, names)
		}
	}
}

// Connector batch entries report their own post under the executor suppression with their own ids
func TestConnectorEntriesReportTheirOwnPostUnderTheExecutorSuppression(t *testing.T) {
	connectorsGate(t, true)
	// The dispatch id reaches the gateway as the tool call header only once telemetry is consented
	consent := TelemetrySendConsented
	TelemetrySendConsented = func(context.Context, *Env) (bool, error) { return true, nil }
	t.Cleanup(func() { TelemetrySendConsented = consent })
	var mu sync.Mutex
	var gatewayIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		var reply map[string]any
		switch {
		case r.URL.Path == "/api/v1/connectors/accounts":
			reply = map[string]any{"accounts": []any{}}
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/execute"):
			mu.Lock()
			id := ""
			for k, v := range r.Header {
				if strings.EqualFold(k, toolCallHeader) && len(v) > 0 {
					id = v[0]
				}
			}
			gatewayIDs = append(gatewayIDs, id)
			mu.Unlock()
			call, _ := body["call"].(map[string]any)
			reply = map[string]any{"result": map[string]any{"connector": call["connector"], "tool": call["tool"], "data": map[string]any{"ok": 1}}}
		default:
			http.NotFound(w, r)
			return
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
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	d := NewDispatcher(Default)
	var posts [][2]string
	d.PostToolCall = append(d.PostToolCall, func(ctx context.Context, env *Env, ev PostToolCallEvent) {
		mu.Lock()
		posts = append(posts, [2]string{ev.ToolName, ev.IDs.ToolCallID})
		mu.Unlock()
	})
	calls := []any{map[string]any{"name": "connectors__gmail__SEND_EMAIL", "arguments": map[string]any{}},
		map[string]any{"name": "connectors__slack__POST", "arguments": map[string]any{}}}
	d.DispatchWith(SuppressPostToolCallHook(context.Background(), true), &Call{Name: ToolCallName, Args: map[string]any{"calls": calls}, ToolCallID: "tc1", Env: env},
		DispatchOptions{EnabledToolsets: []string{"connections"}})
	if want := [][2]string{{"connectors__gmail__SEND_EMAIL", "tc1#0"}, {"connectors__slack__POST", "tc1#1"}}; !reflect.DeepEqual(posts, want) {
		t.Fatalf("posts %v", posts)
	}
	if !reflect.DeepEqual(gatewayIDs, []string{"tc1#0", "tc1#1"}) {
		t.Fatalf("gateway ids %v", gatewayIDs)
	}
}
