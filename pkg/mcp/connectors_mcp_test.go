package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

var connCatalog = []string{"figma", "linear", "notion"}

type fakeAttempt struct {
	mu             sync.Mutex
	authURL        string
	status         string
	err            string
	tools          []string
	discoveryError string
}

func newFakeAttempt(url string) *fakeAttempt { return &fakeAttempt{authURL: url, status: "pending"} }

func (a *fakeAttempt) AuthURL() string           { return a.authURL }
func (a *fakeAttempt) Detail() string            { return "" }
func (a *fakeAttempt) Flow() *DashboardOAuthFlow { return nil }
func (a *fakeAttempt) Poll() AttemptPoll {
	a.mu.Lock()
	defer a.mu.Unlock()
	return AttemptPoll{Status: a.status, Error: a.err, Tools: slices.Clone(a.tools), DiscoveryError: a.discoveryError}
}
func (a *fakeAttempt) approve(names []string) {
	a.mu.Lock()
	a.tools, a.status = slices.Clone(names), "approved"
	a.mu.Unlock()
}

type fakeConnBackend struct {
	mu                sync.Mutex
	calls             [][]any
	attempts          map[string]*fakeAttempt
	missingEnv        []string
	tools             []string
	registeredTools   []string
	installError      string
	registrationError string
	oauthError        string
}

func newFakeConnBackend() *fakeConnBackend {
	return &fakeConnBackend{attempts: map[string]*fakeAttempt{}, tools: []string{"read", "write"}}
}

func (b *fakeConnBackend) record(call ...any) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	b.mu.Unlock()
}

func (b *fakeConnBackend) callList() [][]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

func (b *fakeConnBackend) RequiredEnv(ctx context.Context, name string) ([]*pyjson.Object, error) {
	b.record("required_env", name)
	out := []*pyjson.Object{}
	for _, key := range b.missingEnv {
		o := pyjson.NewObject()
		o.Set("name", key)
		o.Set("prompt", key+"?")
		o.Set("required", true)
		o.Set("secret", true)
		o.Set("default", "")
		out = append(out, o)
	}
	return out, nil
}

func (b *fakeConnBackend) StartOAuth(ctx context.Context, name string) (ConnectorAttempt, error) {
	b.record("start_oauth", name)
	if b.oauthError != "" {
		return nil, &ProbeError{Err: errorString(b.oauthError)}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	a := newFakeAttempt("https://auth.example/" + name + "/" + itoa(len(b.attempts)+1))
	b.attempts[name] = a
	return a, nil
}

func (b *fakeConnBackend) InstallsWithOAuth(ctx context.Context, name string) (bool, error) {
	return false, errorString("FakeBackend has no installs_with_oauth")
}

func (b *fakeConnBackend) StartInstallOAuth(ctx context.Context, name string, env *pyjson.Object) (ConnectorAttempt, error) {
	return nil, errorString("FakeBackend has no start_install_oauth")
}

func (b *fakeConnBackend) Install(ctx context.Context, name string, env *pyjson.Object) ([]string, error) {
	b.record("install", name, envMap(env))
	if b.installError != "" {
		return nil, errorString(b.installError)
	}
	return slices.Clone(b.tools), nil
}

func (b *fakeConnBackend) Enable(ctx context.Context, name string) error {
	b.record("enable", name)
	return nil
}

type errorString string

func (e errorString) Error() string { return string(e) }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func envMap(env *pyjson.Object) map[string]string {
	out := map[string]string{}
	for _, k := range env.Keys {
		out[k], _ = env.Vals[k].(string)
	}
	return out
}

// Swaps the catalog readers, the default backend and the registration step the way the Python fixture patches them
func connFixture(t *testing.T, backend *fakeConnBackend) {
	t.Helper()
	isolateHome(t)
	tools.LiveResetForTests()
	lateMu.Lock()
	clear(lateAttempts)
	lateMu.Unlock()
	saved := []any{connCatalogNames, connConfiguredNames, connDefaultBackend, connRegisterConnected, tools.WatchIntervalSeconds}
	connCatalogNames = func(context.Context) ([]string, error) { return connCatalog, nil }
	connConfiguredNames = func(context.Context) ([]string, error) { return []string{"linear", "paper"}, nil }
	connDefaultBackend = func(*tools.Env) ConnectorBackend { return backend }
	var registered []string
	connRegisterConnected = func(ctx context.Context, r *connRunner, tg *tools.ConnTarget, name string) ([]string, string) {
		fb := r.backend.(*fakeConnBackend)
		fb.record("register", name)
		if fb.registrationError != "" {
			return []string{}, connDetail(r, tg, fb.registrationError)
		}
		for _, toolName := range fb.registeredTools {
			tools.Default.Register(tools.Def{Name: toolName, Toolset: "mcp-" + name,
				Schema:  map[string]any{"name": toolName, "description": "Registered " + toolName, "parameters": map[string]any{}},
				Handler: func(context.Context, *tools.Call) string { return "{}" }})
			registered = append(registered, toolName)
		}
		return slices.Clone(fb.registeredTools), ""
	}
	tools.WatchIntervalSeconds = 0.01
	t.Cleanup(func() {
		connCatalogNames = saved[0].(func(context.Context) ([]string, error))
		connConfiguredNames = saved[1].(func(context.Context) ([]string, error))
		connDefaultBackend = saved[2].(func(*tools.Env) ConnectorBackend)
		connRegisterConnected = saved[3].(func(context.Context, *connRunner, *tools.ConnTarget, string) ([]string, string))
		tools.WatchIntervalSeconds = saved[4].(float64)
		for _, name := range registered {
			_ = tools.Default.Deregister(name, "", "")
		}
		tools.LiveResetForTests()
		lateMu.Lock()
		clear(lateAttempts)
		lateMu.Unlock()
	})
}

type answeringCard struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (c *answeringCard) payloads() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

// A card that emits and answers the live operation a moment later the way connection.respond does
func answering(ctx context.Context, answer *string) (*answeringCard, tools.ConnCallback) {
	card := &answeringCard{}
	return card, func(payload *pyjson.Object) string {
		card.mu.Lock()
		card.seen = append(card.seen, plainJSON(payload))
		card.mu.Unlock()
		opID, _ := payload.Get("op_id")
		if answer != nil {
			time.AfterFunc(10*time.Millisecond, func() {
				if op := tools.LiveGet("s1", opID.(string), ""); op != nil {
					_ = ApplyAnswer(ctx, op, *answer)
				}
			})
		}
		return ""
	}
}

func plainJSON(v any) map[string]any {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// The MCP route of manage_connections over the ported target normalization and validation
func manageMCP(ctx context.Context, args map[string]any, cb tools.ConnCallback, backend ConnectorBackend) map[string]any {
	action, _ := args["action"].(string)
	managed, mcpNames, msg := tools.NormalizeTargets(args["connectors"])
	if msg == "" {
		msg = tools.ValidateAction(action, managed, mcpNames)
	}
	if msg != "" {
		return map[string]any{"error": msg}
	}
	if !slices.Contains(tools.MCPActions, action) {
		panic("managed action reached the managed path: " + action)
	}
	var b any
	if backend != nil {
		b = backend
	}
	out := tools.MCPOperationRunner(ctx, &tools.Env{}, mcpNames, action, tools.MCPOperationOptions{Callback: cb, SessionID: "s1", Backend: b})
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		panic(out)
	}
	return parsed
}

func mcpTarget(name string, extra ...any) map[string]any {
	t := map[string]any{"name": name, "mcp": true}
	for i := 0; i+1 < len(extra); i += 2 {
		t[extra[i].(string)] = extra[i+1]
	}
	return t
}

func strp(s string) *string { return &s }

func firstTarget(out map[string]any) map[string]any {
	return out["targets"].([]any)[0].(map[string]any)
}

func TestConnInstallWaitsForTheCredentialsItDeclaresAndInstallsWithThem(t *testing.T) {
	backend := newFakeConnBackend()
	backend.missingEnv = []string{"FIGMA_TOKEN"}
	backend.tools = []string{"probe_only"}
	backend.registeredTools = []string{"mcp__figma__get_file", "mcp__figma__list_files"}
	connFixture(t, backend)
	ctx := context.Background()
	card, cb := answering(ctx, strp(`{"targets": [{"name": "figma", "status": "approved", "env": {"FIGMA_TOKEN": "tok-1"}}]}`))
	out := manageMCP(ctx, map[string]any{"action": "install", "connectors": []any{mcpTarget("figma")}}, cb, backend)
	offered := card.payloads()[0]["targets"].([]any)
	if len(offered) != 1 || offered[0].(map[string]any)["state"] != "pending" {
		t.Fatalf("offered %v", offered)
	}
	want := []any{map[string]any{"name": "FIGMA_TOKEN", "prompt": "FIGMA_TOKEN?", "required": true, "secret": true, "default": ""}}
	if got, _ := json.Marshal(offered[0].(map[string]any)["required_env"]); string(got) != connJSON(want) {
		t.Fatalf("required_env %s", got)
	}
	found := false
	for _, c := range backend.callList() {
		if c[0] == "install" && c[1] == "figma" && connJSON(c[2]) == `{"FIGMA_TOKEN":"tok-1"}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("calls %v", backend.callList())
	}
	settled := firstTarget(out)
	if settled["state"] != "connected" || connJSON(settled["tools"]) != `["mcp__figma__get_file","mcp__figma__list_files"]` {
		t.Fatalf("settled %v", settled)
	}
	listing, _ := settled["tools_listing"].(string)
	for _, name := range []string{"mcp__figma__get_file", "mcp__figma__list_files", "tool_describe", "tool_call"} {
		if !strings.Contains(listing, name) {
			t.Fatalf("listing misses %s: %q", name, listing)
		}
	}
}

func connJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestConnACardClaimOtherThanApprovedOrSkippedMovesNothing(t *testing.T) {
	backend := newFakeConnBackend()
	connFixture(t, backend)
	ctx := context.Background()
	_, cb := answering(ctx, strp(`{"targets": [{"name": "paper", "status": "connected", "tools": ["x"]}], "settled_by": "continue"}`))
	out := manageMCP(ctx, map[string]any{"action": "enable", "connectors": []any{mcpTarget("paper")}}, cb, backend)
	if len(backend.callList()) != 0 {
		t.Fatalf("calls %v", backend.callList())
	}
	if firstTarget(out)["state"] != "not_connected" || out["settled_by"] != "continue" {
		t.Fatalf("out %v", out)
	}
}

func TestConnNoAnswerSettlesByDeadlineAndMarksTargetsNotConnected(t *testing.T) {
	backend := newFakeConnBackend()
	connFixture(t, backend)
	saved := tools.OperationDeadlineSeconds
	tools.OperationDeadlineSeconds = 0.05
	t.Cleanup(func() { tools.OperationDeadlineSeconds = saved })
	ctx := context.Background()
	_, cb := answering(ctx, nil)
	out := manageMCP(ctx, map[string]any{"action": "install", "connectors": []any{mcpTarget("linear")}}, cb, backend)
	if out["settled_by"] != "deadline" || firstTarget(out)["state"] != "not_connected" {
		t.Fatalf("out %v", out)
	}
	if _, ok := out["error"]; ok {
		t.Fatalf("error in %v", out)
	}
}

func TestConnMCPSecretsNeverReachTheModel(t *testing.T) {
	backend := newFakeConnBackend()
	backend.missingEnv = []string{"LINEAR_API_KEY"}
	backend.registrationError = "sk-secret was rejected"
	connFixture(t, backend)
	ctx := context.Background()
	_, cb := answering(ctx, strp(`{"targets": [{"name": "linear", "status": "approved", "env": {"LINEAR_API_KEY": "sk-secret"}}]}`))
	out := manageMCP(ctx, map[string]any{"action": "install", "connectors": []any{mcpTarget("linear")}}, cb, backend)
	payload := connJSON(out)
	if strings.Contains(payload, "sk-secret") || !strings.Contains(payload, "[REDACTED]") {
		t.Fatalf("payload %s", payload)
	}
	target := firstTarget(out)
	if target["state"] != "connected" || connJSON(target["tools"]) != "[]" || target["discovery_error"] != "[REDACTED] was rejected" {
		t.Fatalf("target %v", target)
	}
}

func TestConnOffDesktopAuthorizeReturnsTheLinkAtOnceAndOpensNoOperation(t *testing.T) {
	backend := newFakeConnBackend()
	connFixture(t, backend)
	out := manageMCP(context.Background(), map[string]any{"action": "authorize", "connectors": []any{mcpTarget("paper")}}, nil, backend)
	target := firstTarget(out)
	if target["state"] != "initiated" || target["connect_url"] != "https://auth.example/paper/1" || out["status"] != "initiated" {
		t.Fatalf("out %v", out)
	}
	if tools.LiveCurrent("s1", "") != nil {
		t.Fatal("an operation was opened")
	}
}

func TestConnAManagedActionNeverAcceptsMCPTargetsAndViceVersa(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	ctx := context.Background()
	out := manageMCP(ctx, map[string]any{"action": "connect", "connectors": []any{"gmail", mcpTarget("linear")}}, nil, nil)
	if !strings.Contains(out["error"].(string), "managed-connector action") {
		t.Fatalf("out %v", out)
	}
	out = manageMCP(ctx, map[string]any{"action": "install", "connectors": []any{"gmail", mcpTarget("linear")}}, nil, nil)
	if !strings.Contains(out["error"].(string), "must carry") {
		t.Fatalf("out %v", out)
	}
}

func TestConnUnknownTargetFieldsAreRejected(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	out := manageMCP(context.Background(), map[string]any{"action": "install", "connectors": []any{mcpTarget("linear", "url", "https://evil")}}, nil, nil)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "unknown target field") || !strings.Contains(msg, "url") {
		t.Fatalf("out %v", out)
	}
}

func TestConnInstallIsCatalogOnlyAndListsTheCatalogOnAMiss(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	out := manageMCP(context.Background(), map[string]any{"action": "install", "connectors": []any{mcpTarget("github")}}, nil, nil)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "github") || !strings.Contains(msg, "figma, linear, notion") {
		t.Fatalf("out %v", out)
	}
}

func TestConnEnableAndAuthorizeNeedAConfiguredServer(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	out := manageMCP(context.Background(), map[string]any{"action": "enable", "connectors": []any{mcpTarget("figma")}}, nil, nil)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "figma") || !strings.Contains(msg, "paper") {
		t.Fatalf("out %v", out)
	}
}

func TestConnADesktopSessionWithNoCallbackGetsTheLinkAtOnceAndOpensNoOperation(t *testing.T) {
	backend := newFakeConnBackend()
	connFixture(t, backend)
	saved := tools.OperationDeadlineSeconds
	tools.OperationDeadlineSeconds = 0.2
	t.Cleanup(func() { tools.OperationDeadlineSeconds = saved })
	out := manageMCP(context.Background(), map[string]any{"action": "authorize", "connectors": []any{mcpTarget("paper")}}, nil, backend)
	if out["status"] != "initiated" || firstTarget(out)["connect_url"] != "https://auth.example/paper/1" {
		t.Fatalf("out %v", out)
	}
	if tools.LiveCurrent("s1", "") != nil {
		t.Fatal("an operation was opened")
	}
}

type lateAgent struct {
	session  string
	enabled  []string
	disabled []string
}

func (a *lateAgent) SessionID() string          { return a.session }
func (a *lateAgent) EnabledToolsets() []string  { return a.enabled }
func (a *lateAgent) DisabledToolsets() []string { return a.disabled }
func (a *lateAgent) SetToolsets(enabled, disabled []string) {
	a.enabled, a.disabled = enabled, disabled
}
func (a *lateAgent) ToolEnv() *tools.Env { return &tools.Env{} }

// A real profile dir with the same named MCP server configured
func connProfileHome(t *testing.T, root, name string) string {
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mcp_servers:\n  linear:\n    url: https://mcp.linear.app/mcp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Closes a runner whose operation parked one approved OAuth attempt, bound to dir
func parkAttempt(dir string, stamped bool) {
	attempt := newFakeAttempt("https://auth.example/linear")
	attempt.approve([]string{"read"})
	op := tools.NewConnectionOperation(nil, "s1", "", nil)
	r := openRunner(&tools.Env{}, "authorize", newFakeConnBackend())
	r.operation = op
	r.work = map[string]*connWork{"linear": {attempt: attempt}}
	ctx := home.WithOverride(context.Background(), dir)
	if stamped {
		op.ProfileKey = home.Key(home.Dir(ctx))
	}
	r.close(ctx)
}

// Adopt late connections inside a turn bound to dir, with server registration recorded
func adoptAs(t *testing.T, dir string, registered *[]string) ([]string, *lateAgent) {
	saved := connRegisterServers
	connRegisterServers = func(ctx context.Context, env *tools.Env, servers map[string]*Rendered) ([]string, error) {
		for name := range servers {
			*registered = append(*registered, name)
		}
		return nil, nil
	}
	defer func() { connRegisterServers = saved }()
	agent := &lateAgent{session: "s1", enabled: []string{}}
	return AdoptLateConnections(home.WithOverride(context.Background(), dir), agent), agent
}

func lateCount() int {
	lateMu.Lock()
	defer lateMu.Unlock()
	return len(lateAttempts)
}

func TestConnLateAttemptIsNeverAdoptedByAnotherProfile(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	root := t.TempDir()
	homeA, homeB := connProfileHome(t, root, "home-a"), connProfileHome(t, root, "home-b")
	parkAttempt(homeA, true)
	var registered []string
	adopted, _ := adoptAs(t, homeB, &registered)
	if len(adopted) != 0 || len(registered) != 0 || lateCount() == 0 {
		t.Fatalf("adopted %v registered %v", adopted, registered)
	}
	adopted, agent := adoptAs(t, homeA, &registered)
	if connJSON(adopted) != `["linear"]` || connJSON(registered) != `["linear"]` || connJSON(agent.enabled) != `["linear"]` || lateCount() != 0 {
		t.Fatalf("adopted %v registered %v agent %v", adopted, registered, agent.enabled)
	}
}

func TestConnLateAttemptKeyedByDetachedPathUsesCallingProfile(t *testing.T) {
	connFixture(t, newFakeConnBackend())
	root := t.TempDir()
	homeA, homeB := connProfileHome(t, root, "home-a"), connProfileHome(t, root, "home-b")
	parkAttempt(homeA, false)
	lateMu.Lock()
	_, ok := lateAttempts[connLateKeyT{home.Key(homeA), "s1"}]
	n := len(lateAttempts)
	lateMu.Unlock()
	if !ok || n != 1 {
		t.Fatalf("parked under the wrong key")
	}
	var registered []string
	if adopted, _ := adoptAs(t, homeB, &registered); len(adopted) != 0 {
		t.Fatalf("adopted %v", adopted)
	}
	if adopted, _ := adoptAs(t, homeA, &registered); connJSON(adopted) != `["linear"]` {
		t.Fatalf("adopted %v", adopted)
	}
}

func TestConnE2ECardedOAuthParkAndAdoptStayInsideTheirProfile(t *testing.T) {
	backend := newFakeConnBackend()
	connFixture(t, backend)
	root := t.TempDir()
	homeA, homeB := connProfileHome(t, root, "home-a"), connProfileHome(t, root, "home-b")
	saved := tools.OperationDeadlineSeconds
	tools.OperationDeadlineSeconds = 0.05
	ctxA := home.WithOverride(context.Background(), homeA)
	_, cb := answering(ctxA, nil)
	out := manageMCP(ctxA, map[string]any{"action": "authorize", "connectors": []any{mcpTarget("linear")}}, cb, backend)
	tools.OperationDeadlineSeconds = saved
	if out["settled_by"] != "deadline" {
		t.Fatalf("out %v", out)
	}
	backend.mu.Lock()
	backend.attempts["linear"].approve([]string{"read"})
	backend.mu.Unlock()
	var registered []string
	if adopted, agent := adoptAs(t, homeB, &registered); len(adopted) != 0 || len(registered) != 0 || len(agent.enabled) != 0 {
		t.Fatalf("home b adopted %v", adopted)
	}
	if lateCount() == 0 {
		t.Fatal("the attempt was not kept for its owner")
	}
	adopted, agent := adoptAs(t, homeA, &registered)
	if connJSON(adopted) != `["linear"]` || connJSON(registered) != `["linear"]` || connJSON(agent.enabled) != `["linear"]` || lateCount() != 0 {
		t.Fatalf("adopted %v registered %v agent %v", adopted, registered, agent.enabled)
	}
}
