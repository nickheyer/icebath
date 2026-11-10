package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Gateway double answering execute requests with fixed raw JSON and keeping every execute request body
func rawConnectorGateway(t *testing.T, executeReply string) *[]string {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/connectors/accounts":
			io.WriteString(w, `{"accounts": []}`)
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/execute"):
			mu.Lock()
			*bodies = append(*bodies, string(body))
			mu.Unlock()
			io.WriteString(w, executeReply)
		case strings.HasSuffix(r.URL.Path, "/v1/connectors/schemas"), strings.HasSuffix(r.URL.Path, "/v1/connectors/search"):
			io.WriteString(w, `{"schemas": {}, "results": []}`)
		case strings.HasSuffix(r.URL.Path, "/v1/connectors") && r.Method == http.MethodGet:
			io.WriteString(w, `{"items": []}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "t")
	oldPortal := NousPortalBaseURL
	NousPortalBaseURL = func(context.Context) (string, error) { return srv.URL, nil }
	t.Cleanup(func() { NousPortalBaseURL = oldPortal })
	return bodies
}

// One model tool call carrying the model's raw argument text, its result reloaded and dumped as json.dumps would
func rawArgsCall(t *testing.T, env *Env, name, raw string) string {
	t.Helper()
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		t.Fatal(err)
	}
	opts := DispatchOptions{EnabledToolsets: []string{"connections"}, SkipPreToolCallHook: true, SkipToolRequestMiddleware: true, SkipToolExecutionMiddleware: true}
	out := DispatcherOf(context.Background()).DispatchWith(context.Background(), &Call{Name: name, Args: pyjson.WTF8Shape(decoded).(map[string]any), RawArgs: raw, SessionID: "s", Env: env}, opts)
	reloaded, err := pyjson.Loads(out)
	if err != nil {
		t.Fatalf("%s: %v in %s", name, err, out)
	}
	text, err := pyjson.Dumps(reloaded, pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestConnectorCallKeepsNumberTypesAndKeyOrderBothWays(t *testing.T) {
	connectorsGate(t, true)
	bodies := rawConnectorGateway(t, `{"result": {"connector": "gmail", "tool": "GMAIL_FETCH_EMAILS", "data": {"zeta": 1, "alpha": 1.0, "big": 1e2, "list": [1, 1.0, {"y": 2, "x": 2.0}]}, "account": "work"}}`)
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	args := `{"n": 1, "f": 1.0, "s": "é", "nested": {"b": 2, "a": [1, 2.0]}}`
	single := rawArgsCall(t, env, "connectors__gmail__FETCH_EMAILS", args)
	if want := `{"response": {"zeta": 1, "alpha": 1.0, "big": 100.0, "list": [1, 1.0, {"y": 2, "x": 2.0}]}, "account": "work"}`; single != want {
		t.Fatalf("single call result %s\nwant %s", single, want)
	}
	wantArgs := `"arguments": {"n": 1, "f": 1.0, "s": "\u00e9", "nested": {"b": 2, "a": [1, 2.0]}}}`
	if len(*bodies) != 1 || !strings.HasPrefix((*bodies)[0], `{"call": {"connector": "gmail", "tool": "`) || !strings.Contains((*bodies)[0], wantArgs) {
		t.Fatalf("execute request bodies %q, want the call object first and %s", *bodies, wantArgs)
	}
	*bodies = nil
	batch := rawArgsCall(t, env, "tool_call", `{"calls": [{"name": "connectors__gmail__FETCH_EMAILS", "arguments": `+args+`}, {"name": "connectors__gmail__FETCH_EMAILS", "arguments": {}}]}`)
	entry := `{"index": %d, "name": "connectors__gmail__FETCH_EMAILS", "response": {"zeta": 1, "alpha": 1.0, "big": 100.0, "list": [1, 1.0, {"y": 2, "x": 2.0}]}, "account": "work"}`
	want := `{"results": [` + strings.Replace(entry, "%d", "0", 1) + `, ` + strings.Replace(entry, "%d", "1", 1) + `], "success_count": 2, "error_count": 0, "total_count": 2}`
	if batch != want {
		t.Fatalf("batch result %s\nwant %s", batch, want)
	}
	if len(*bodies) != 2 || !strings.Contains((*bodies)[0], wantArgs) || !strings.Contains((*bodies)[1], `"arguments": {}}`) {
		t.Fatalf("batch execute request bodies %q", *bodies)
	}
}

func TestConnectorRequestBodiesAreTheWireModelDumps(t *testing.T) {
	connectorsGate(t, true)
	var mu sync.Mutex
	bodies := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CONNECTOR_GATEWAY_URL", srv.URL)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "t")
	env := &Env{Home: t.TempDir()}
	t.Setenv("ICEBATH_HOME", env.Home)
	ctx := context.Background()
	c := NewConnectorClient(env, nil, "")
	c.Search(ctx, []map[string]any{{"use_case": "find é", "known_fields": "k"}, {"use_case": "x"}})
	c.Schemas(ctx, []string{"GMAIL_SEND_EMAIL", "B"})
	c.Connections(ctx, ConnectionsRequest{Connectors: []string{"gmail"}, Alias: "work", ConnectionID: "c1", ReturnTo: "cli", Op: "op1"})
	c.StageFile(ctx, "gmail", "T", "a<b>.txt", "text/plain", 3)
	for route, want := range map[string]string{
		"search":      `{"queries": [{"useCase": "find \u00e9", "knownFields": "k"}, {"useCase": "x"}]}`,
		"schemas":     `{"tools": ["GMAIL_SEND_EMAIL", "B"]}`,
		"connections": `{"connectors": ["gmail"], "reinitiate": false, "alias": "work", "connectionId": "c1", "returnTo": "cli", "op": "op1"}`,
		"upload":      `{"toolkit": "gmail", "tool": "T", "filename": "a<b>.txt", "mimetype": "text/plain", "size": 3}`,
	} {
		if bodies[route] != want {
			t.Errorf("%s request body %q, want %q", route, bodies[route], want)
		}
	}
}
