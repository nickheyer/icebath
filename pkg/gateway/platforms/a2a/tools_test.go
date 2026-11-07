package a2a

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Swaps the config loader and both HTTP seams for one test, restoring them afterwards
func swapToolSeams(t *testing.T, cfg map[string]any, get func(string) (any, error), post func(string, *pyjson.Object) (any, error)) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	prevCfg, prevGet, prevPost := loadToolsConfig, httpGetJSON, httpPostJSON
	t.Cleanup(func() { loadToolsConfig, httpGetJSON, httpPostJSON = prevCfg, prevGet, prevPost })
	if cfg != nil {
		loadToolsConfig = func(context.Context) (map[string]any, error) { return cfg, nil }
	}
	if get != nil {
		httpGetJSON = func(_ context.Context, url string, _ map[string]string, _ int) (any, error) { return get(url) }
	}
	if post != nil {
		httpPostJSON = func(_ context.Context, url string, body *pyjson.Object, _ map[string]string, _ int) (any, error) {
			return post(url, body)
		}
	}
}

func nilCard(string) (any, error) { return nil, nil }

func peers(names ...string) map[string]any {
	m := map[string]any{}
	for _, n := range names {
		m[n] = map[string]any{"url": "http://localhost:9999"}
	}
	return map[string]any{"a2a_agents": m}
}

func TestA2AHistoryToolRecallsConversation(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	PersistMessage(ctx, "ctx-recall", "user", "what is 2+2", "t1", "")
	PersistMessage(ctx, "ctx-recall", "agent", "4", "t1", "")
	out := A2AHistory(ctx, map[string]any{"context_id": "ctx-recall"})
	if !strings.Contains(out, "what is 2+2") || !strings.Contains(out, "[agent] 4") {
		t.Fatalf("out %q", out)
	}
	PersistMessage(ctx, "ctx-long", "agent", strings.Repeat("x", 1200), "t2", "")
	if out := A2AHistory(ctx, map[string]any{"contextId": "ctx-long", "limit": "bad"}); !strings.Contains(out, strings.Repeat("x", 1000)+" …[truncated]") {
		t.Fatalf("long out %q", out)
	}
	if out := A2AHistory(ctx, map[string]any{"context_id": "nope"}); out != "No persisted conversation for context 'nope'." {
		t.Fatalf("missing out %q", out)
	}
}

func TestDiscoverSummarizesV1Card(t *testing.T) {
	card := BuildAgentCard(AgentCardOptions{Name: "researcher", URL: "http://localhost:9999/", Description: "finds things",
		Skills: []any{pyjson.ObjectOf("id", "s", "name", "search", "description", "web search")}})
	swapToolSeams(t, nil, func(string) (any, error) { return card, nil }, nil)
	out := A2ADiscover(context.Background(), map[string]any{"url": "http://localhost:9999"})
	if !strings.Contains(out, "researcher") || !strings.Contains(out, "search") || !strings.Contains(out, "Protocol: JSONRPC v1.0") {
		t.Fatalf("out %q", out)
	}
}

func TestCallSendsV1Message(t *testing.T) {
	var captured *pyjson.Object
	swapToolSeams(t, map[string]any{"a2a_agents": map[string]any{"r": map[string]any{"url": "http://localhost:9999"}}}, nilCard,
		func(_ string, body *pyjson.Object) (any, error) {
			captured = body
			id, _ := body.Get("id")
			ctxID, _ := path(t, body, "params", "message").(*pyjson.Object).Get("contextId")
			return JsonrpcResult(id, BuildTask("t", ctxID.(string), StateCompleted, "here is the answer")), nil
		})
	out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "my key sk-abcdefghij1234567890ABCD please"})
	if !strings.Contains(out, "here is the answer") {
		t.Fatalf("out %q", out)
	}
	params := path(t, captured, "params").(*pyjson.Object)
	if _, ok := params.Get("contextId"); ok {
		t.Fatal("contextId must not be top level")
	}
	if path(t, params, "message", "contextId") == "" || path(t, params, "message", "role") != "ROLE_USER" {
		t.Fatalf("message %v", path(t, params, "message"))
	}
	part := path(t, params, "message", "parts", 0).(*pyjson.Object)
	if _, ok := part.Get("kind"); ok || path(t, part, "mediaType") != "text/plain" {
		t.Fatalf("part %v", part)
	}
	if strings.Contains(path(t, part, "text").(string), "sk-abcdefghij") {
		t.Fatal("outbound text not redacted")
	}
}

func TestCallReportsInputRequired(t *testing.T) {
	swapToolSeams(t, peers("r"), nilCard, func(_ string, body *pyjson.Object) (any, error) {
		id, _ := body.Get("id")
		return JsonrpcResult(id, BuildTask("t", "ctx-q", StateInputRequired, "Which repo?")), nil
	})
	out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "review the code"})
	for _, part := range []string{"Which repo?", "input-required", "ctx-q", "[r · context ctx-q · input-required]"} {
		if !strings.Contains(out, part) {
			t.Fatalf("missing %q in %q", part, out)
		}
	}
}

func TestRPCURLPrefersSupportedInterfaces(t *testing.T) {
	card := mustLoads(t, `{"url": "http://legacy:1/", "supportedInterfaces": [{"url": "http://v1:2/", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}]}`)
	if got := rpcURL("http://base:3", card); got != "http://v1:2/" {
		t.Fatalf("got %q", got)
	}
	if got := rpcURL("http://base:3", mustLoads(t, `{"url": "http://legacy:1/"}`)); got != "http://legacy:1/" {
		t.Fatalf("got %q", got)
	}
	if got := rpcURL("http://base:3/", nil); got != "http://base:3" {
		t.Fatalf("got %q", got)
	}
}

func TestRegisterThenDispatchViaRegistry(t *testing.T) {
	swapToolSeams(t, map[string]any{}, nil, nil)
	specs := map[string]func(context.Context, *tools.Call) string{}
	for _, spec := range toolSpecs() {
		specs[spec.Name] = spec.Handler
	}
	ctx := context.Background()
	for name, args := range map[string]map[string]any{"a2a_discover": {"url": ""}, "a2a_call": {"agent": "", "message": ""},
		"a2a_history": {}} {
		if out := specs[name](ctx, &tools.Call{Name: name, Args: args}); !strings.Contains(out, "required") || strings.Contains(out, "AttributeError") {
			t.Fatalf("%s out %q", name, out)
		}
	}
	if out := specs["a2a_list"](ctx, &tools.Call{Name: "a2a_list", Args: map[string]any{}}); !strings.Contains(out, "No peers configured") {
		t.Fatalf("list out %q", out)
	}
}

func TestA2ACallAcceptsAgentNameAlias(t *testing.T) {
	sent := false
	swapToolSeams(t, peers("peer"), nilCard, func(_ string, body *pyjson.Object) (any, error) {
		sent = true
		id, _ := body.Get("id")
		return JsonrpcResult(id, BuildTask("t", "c1", StateCompleted, "PONG")), nil
	})
	out := A2ACall(context.Background(), map[string]any{"agent_name": "peer", "message": "ping"})
	if !sent || !strings.Contains(out, "PONG") {
		t.Fatalf("sent %v out %q", sent, out)
	}
}

func devCard(t *testing.T) func(string) (any, error) {
	return func(url string) (any, error) {
		if !strings.HasSuffix(url, "/.well-known/agent-card.json") {
			t.Fatalf("card url %q", url)
		}
		return BuildAgentCard(AgentCardOptions{Name: "dev", URL: "http://peer.example/dev/", Description: "dev", Tenant: "dev-team"}), nil
	}
}

func TestRPCBodyEchoesTenantFromAgentCard(t *testing.T) {
	var postedURL string
	var posted *pyjson.Object
	swapToolSeams(t, nil, devCard(t), func(url string, body *pyjson.Object) (any, error) {
		postedURL, posted = url, body
		id, _ := body.Get("id")
		return pyjson.ObjectOf("jsonrpc", "2.0", "id", id, "result", BuildTask("task-1", "ctx-1", StateCompleted, "ok")), nil
	})
	reply, _, _, err := sendTask(context.Background(), "dev", peerSpec{URL: "http://peer.example", Auth: map[string]any{}, Timeout: 5}, "hello", "ctx-1")
	if err != nil || reply != "ok" || postedURL != "http://peer.example/dev/" || path(t, posted, "params", "tenant") != "dev-team" {
		t.Fatalf("reply %q err %v url %q", reply, err, postedURL)
	}
}

func TestDiscoveryFallsBackToLegacyAgentJSON(t *testing.T) {
	var calls []string
	swapToolSeams(t, nil, func(url string) (any, error) {
		calls = append(calls, url)
		if strings.HasSuffix(url, "agent-card.json") {
			return nil, &HTTPError{Code: 404, Reason: "not found"}
		}
		return BuildAgentCard(AgentCardOptions{Name: "legacy", URL: "http://legacy/", Description: "legacy"}), nil
	}, nil)
	out := A2ADiscover(context.Background(), map[string]any{"url": "http://legacy"})
	if !strings.Contains(out, "Agent: legacy") || !strings.HasSuffix(calls[0], "/.well-known/agent-card.json") ||
		!strings.HasSuffix(calls[1], "/.well-known/agent.json") {
		t.Fatalf("out %q calls %v", out, calls)
	}
}

func TestClientSendsV1MethodAndUnwrapsResponse(t *testing.T) {
	var posted *pyjson.Object
	swapToolSeams(t, nil, devCard(t), func(_ string, body *pyjson.Object) (any, error) {
		posted = body
		id, _ := body.Get("id")
		return pyjson.ObjectOf("jsonrpc", "2.0", "id", id, "result",
			pyjson.ObjectOf("task", BuildTask("task-1", "ctx-1", StateCompleted, "ok"))), nil
	})
	reply, _, state, err := sendTask(context.Background(), "dev", peerSpec{URL: "http://peer.example", Auth: map[string]any{}, Timeout: 5}, "hello", "ctx-1")
	if err != nil || reply != "ok" || state != StateCompleted || path(t, posted, "method") != "SendMessage" || path(t, posted, "params", "tenant") != "dev-team" {
		t.Fatalf("reply %q state %q err %v", reply, state, err)
	}
}

func TestCallMapsHTTPAndPeerErrors(t *testing.T) {
	for code, want := range map[int]string{401: "Error: peer 'r' rejected auth (HTTP 401). Check the configured token.",
		429: "Error: peer 'r' rate limited us (HTTP 429). Retry later.", 500: "Error: call to 'r' failed — HTTP 500."} {
		swapToolSeams(t, peers("r"), nilCard, func(string, *pyjson.Object) (any, error) { return nil, &HTTPError{Code: code, Reason: "x"} })
		if out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "m"}); out != want {
			t.Fatalf("code %d out %q", code, out)
		}
	}
	swapToolSeams(t, peers("r"), nilCard, func(string, *pyjson.Object) (any, error) {
		return mustLoads(t, `{"error": {"code": -1, "message": "nope"}}`), nil
	})
	if out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "m"}); out != "Peer 'r' returned an error: nope" {
		t.Fatalf("out %q", out)
	}
	swapToolSeams(t, peers("r"), nilCard, func(string, *pyjson.Object) (any, error) { return nil, errors.New("boom") })
	if out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "m"}); out != "Error: call to 'r' failed — boom." {
		t.Fatalf("out %q", out)
	}
	if out := A2ACall(context.Background(), map[string]any{"agent": "ghost", "message": "m"}); !strings.HasPrefix(out, "Error: unknown agent 'ghost'.") {
		t.Fatalf("out %q", out)
	}
}

func TestHTTPJSONAgainstALivePeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/agent-card.json":
			http.NotFound(w, r)
		case "/.well-known/agent.json":
			w.Write([]byte(`{"name": "old", "skills": [{"id": "x"}]}`))
		default:
			if r.Header.Get("A2A-Version") != ProtocolVersion || r.Header.Get("Authorization") != "Bearer tk" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte("\xff"))
		}
	}))
	defer srv.Close()
	swapToolSeams(t, map[string]any{"a2a_agents": map[string]any{"r": map[string]any{"url": srv.URL, "auth": map[string]any{"type": "bearer", "token": "tk"}}}}, nil, nil)
	out := A2ADiscover(context.Background(), map[string]any{"url": srv.URL})
	if !strings.Contains(out, "Agent: old") || !strings.Contains(out, "Protocol: v? (pre-1.0 card)") || !strings.Contains(out, "  - x: ") {
		t.Fatalf("discover %q", out)
	}
	if out := A2ACall(context.Background(), map[string]any{"agent": "r", "message": "m"}); !strings.Contains(out, "'utf-8' codec can't decode byte 0xff") {
		t.Fatalf("call %q", out)
	}
}

var twoPeers = map[string]any{"a2a_agents": func() map[string]any {
	m := map[string]any{"researcher": map[string]any{"url": "http://localhost:9991", "capabilities": []any{"research"}},
		"coder":      map[string]any{"url": "http://localhost:9992", "capabilities": []any{"code"}},
		"generalist": map[string]any{"url": "http://localhost:9993", "capabilities": []any{"research", "code"}}}
	config.SetMappingKeys(m, []string{"researcher", "coder", "generalist"})
	return m
}()}

// Replaces the per peer call with a canned reply table for one test
func swapPeerCalls(t *testing.T, reply func(name string) string) {
	t.Helper()
	swapToolSeams(t, twoPeers, nil, nil)
	prev := callPeerSync
	t.Cleanup(func() { callPeerSync = prev })
	callPeerSync = func(_ context.Context, name string, _ any, _, _ string) (string, string) { return name, reply(name) }
}

func TestRequiresCapabilityAndMessage(t *testing.T) {
	ctx := context.Background()
	if out := A2AOrchestrate(ctx, map[string]any{"message": "do something"}); !strings.Contains(out, "capability") {
		t.Fatalf("out %q", out)
	}
	if out := A2AOrchestrate(ctx, map[string]any{"capability": "research"}); !strings.Contains(out, "message") {
		t.Fatalf("out %q", out)
	}
}

func TestMatchPeersByCapability(t *testing.T) {
	swapToolSeams(t, twoPeers, nil, nil)
	got := matchPeersByCapability(context.Background(), "research")
	if len(got) != 2 || got[0].name != "researcher" || got[1].name != "generalist" {
		t.Fatalf("matches %v", got)
	}
	if n := len(matchPeersByCapability(context.Background(), "*")); n != 3 {
		t.Fatalf("star matched %d", n)
	}
}

func TestAllModeReturnsEveryReply(t *testing.T) {
	swapPeerCalls(t, func(name string) string { return "reply from " + name })
	out := A2AOrchestrate(context.Background(), map[string]any{"capability": "research", "message": "go"})
	want := "Orchestrated 'research' to 2 peer(s):\n\n--- generalist ---\nreply from generalist\n\n--- researcher ---\nreply from researcher"
	if out != want {
		t.Fatalf("out %q", out)
	}
}

func TestBestModePicksLongestSuccess(t *testing.T) {
	replies := map[string]string{"researcher": "short", "generalist": "a much longer and more detailed reply"}
	swapPeerCalls(t, func(name string) string { return replies[name] })
	if out := A2AOrchestrate(context.Background(), map[string]any{"capability": "research", "message": "go", "mode": "best"}); !strings.HasPrefix(out, "[best: generalist]") {
		t.Fatalf("out %q", out)
	}
}

func TestBestModeIgnoresErrorReplies(t *testing.T) {
	replies := map[string]string{"researcher": "ok", "generalist": "Error: " + strings.Repeat("x", 500)}
	swapPeerCalls(t, func(name string) string { return replies[name] })
	out := A2AOrchestrate(context.Background(), map[string]any{"capability": "research", "message": "go", "mode": "best"})
	if !strings.HasPrefix(out, "[best: researcher]") || !strings.Contains(out, "ok") {
		t.Fatalf("out %q", out)
	}
}

func TestBestModeAllErrorsReportsFailure(t *testing.T) {
	swapPeerCalls(t, func(string) string { return "Error: connection refused" })
	out := A2AOrchestrate(context.Background(), map[string]any{"capability": "research", "message": "go", "mode": "best"})
	if !strings.HasPrefix(out, "All peers failed:") || strings.Contains(out, "[best:") {
		t.Fatalf("out %q", out)
	}
}

func TestFirstModeAllErrorsReportsFailure(t *testing.T) {
	swapPeerCalls(t, func(string) string { return "Error: nope" })
	if out := A2AOrchestrate(context.Background(), map[string]any{"capability": "code", "message": "go", "mode": "first"}); !strings.HasPrefix(out, "All peers failed:") {
		t.Fatalf("out %q", out)
	}
}

func TestFirstModeReturnsASuccess(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	swapPeerCalls(t, func(name string) string {
		mu.Lock()
		calls++
		mu.Unlock()
		return "win " + name
	})
	out := A2AOrchestrate(context.Background(), map[string]any{"capability": "code", "message": "go", "mode": "first"})
	if !strings.HasPrefix(out, "[first: ") || !strings.Contains(out, "win") {
		t.Fatalf("out %q", out)
	}
}

// Availability verdict of the gate with the config loader returning cfg or failing
func gateWith(t *testing.T, cfg map[string]any, fail bool) bool {
	t.Helper()
	prev := loadToolsConfig
	t.Cleanup(func() { loadToolsConfig = prev })
	loadToolsConfig = func(context.Context) (map[string]any, error) {
		if fail {
			return nil, errors.New("boom")
		}
		return cfg, nil
	}
	dir := t.TempDir()
	return a2aToolsAvailable(&tools.Env{Home: dir, Secrets: config.NewSecrets(dir)})
}

func TestUnconfiguredInstallServesNothing(t *testing.T) {
	os.Unsetenv("A2A_PORT")
	if gateWith(t, map[string]any{}, false) || gateWith(t, map[string]any{"a2a_agents": map[string]any{}}, false) {
		t.Fatal("unconfigured install served the tools")
	}
}

func TestPeersConfiguredServes(t *testing.T) {
	if !gateWith(t, map[string]any{"a2a_agents": map[string]any{"r": map[string]any{"url": "http://x"}}}, false) {
		t.Fatal("configured peers did not serve")
	}
}

func TestInboundPlatformEnabledServes(t *testing.T) {
	if !gateWith(t, map[string]any{"platforms": map[string]any{"a2a": map[string]any{"enabled": true}}}, false) {
		t.Fatal("enabled platform did not serve")
	}
	if gateWith(t, map[string]any{"platforms": []any{"a2a"}}, false) {
		t.Fatal("malformed platforms must fail closed")
	}
}

func TestA2APortEnvServes(t *testing.T) {
	t.Setenv("A2A_PORT", "9999")
	if !gateWith(t, map[string]any{}, false) {
		t.Fatal("A2A_PORT did not serve")
	}
}

func TestConfigCrashFailsClosed(t *testing.T) {
	os.Unsetenv("A2A_PORT")
	if gateWith(t, nil, true) {
		t.Fatal("config crash served the tools")
	}
}

func TestAllFiveToolsCarryTheGate(t *testing.T) {
	specs := toolSpecs()
	if len(specs) != 5 {
		t.Fatalf("specs %d", len(specs))
	}
	want := []string{"a2a_discover", "a2a_call", "a2a_list", "a2a_history", "a2a_orchestrate"}
	for i, spec := range specs {
		if spec.Name != want[i] || spec.Toolset != "a2a" || spec.Check == nil || spec.Emoji != "\U0001f9e9" {
			t.Fatalf("spec %+v", spec)
		}
	}
}

func TestSecondaryProfileToolsGateFollowsItsOwnScope(t *testing.T) {
	t.Setenv("A2A_PORT", "9902")
	prev := loadToolsConfig
	t.Cleanup(func() { loadToolsConfig = prev; tools.SetMultiplexActive(false) })
	loadToolsConfig = func(context.Context) (map[string]any, error) { return map[string]any{}, nil }
	root := t.TempDir()
	homeA, homeB := filepath.Join(root, "s6probe-a"), filepath.Join(root, "s6probe-b")
	for dir, env := range map[string]string{homeA: "A2A_PORT=9911\n", homeB: "OTHER=1\n"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools.SetMultiplexActive(true)
	var seen []bool
	for _, h := range []string{homeA, homeB, homeA} {
		seen = append(seen, a2aToolsAvailable(&tools.Env{Home: h, Secrets: config.NewSecrets(h)}))
	}
	if !seen[0] || seen[1] || !seen[2] {
		t.Fatalf("seen %v", seen)
	}
}
