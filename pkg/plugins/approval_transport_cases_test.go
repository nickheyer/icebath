package plugins

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Manifest of the hermes fixture-approval plugin
func atManifest(name string) *Manifest {
	return &Manifest{Name: name, Version: "1.0.0", Description: "fixture", Source: "user", Key: name}
}

// Context of a fixture plugin on a manager
func atContext(m *Manager, name string) *Context { return NewContext(atManifest(name), m) }

// Transport answering every request with one choice
func atRespond(choice string) tools.ApprovalPresentFunc {
	return func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
		return req.Respond(choice), nil
	}
}

// Transport recording every request it saw before answering once
func atRecording(mu *sync.Mutex, seen *[]tools.ApprovalRequest, decide func(tools.ApprovalRequest) tools.ApprovalTransportDecision) tools.ApprovalPresentFunc {
	return func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
		mu.Lock()
		*seen = append(*seen, req)
		mu.Unlock()
		return decide(req), nil
	}
}

func TestContextRegistersOwnedApprovalTransport(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := homeCtx(m.home)
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRespond("deny")); err != nil {
		t.Fatal(err)
	}
	registered := m.GetApprovalTransport(ctx, "phone")
	if registered == nil || registered.Name != "phone" || registered.PluginID != "fixture-approval" {
		t.Fatalf("registered %+v", registered)
	}
	decision, err := registered.Present(ctx, tools.ApprovalRequest{RequestID: "r", Digest: "d"})
	if err != nil || decision.Choice != "deny" || decision.RequestID != "r" {
		t.Fatalf("present %+v %v", decision, err)
	}
}

func TestTransportNamesAreUniqueAndBuiltinIsReserved(t *testing.T) {
	m := NewManager(t.TempDir())
	ctx := homeCtx(m.home)
	if err := atContext(m, "first").RegisterApprovalTransport(ctx, "phone", atRespond("deny")); err != nil {
		t.Fatal(err)
	}
	if err := atContext(m, "second").RegisterApprovalTransport(ctx, "phone", atRespond("deny")); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate %v", err)
	}
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "builtin", atRespond("deny")); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("builtin %v", err)
	}
}

func TestForceReloadClearsTransportRegistry(t *testing.T) {
	ledgerSwapBundled(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	m := NewManager(h)
	ctx := homeCtx(h)
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRespond("deny")); err != nil {
		t.Fatal(err)
	}
	m.DiscoverAndLoad(ctx, true)
	if m.GetApprovalTransport(ctx, "phone") != nil {
		t.Fatal("a force reload keeps the old transport")
	}
}

func TestTransportRegistryIsManagerAndProfileIsolated(t *testing.T) {
	tmp := t.TempDir()
	work := homeCtx(filepath.Join(tmp, "profiles", "work"))
	first, second := NewManager(filepath.Join(tmp, "a")), NewManager(filepath.Join(tmp, "b"))
	if err := atContext(first, "fixture-approval").RegisterApprovalTransport(work, "phone", atRespond("deny")); err != nil {
		t.Fatal(err)
	}
	if first.GetApprovalTransport(work, "phone") == nil {
		t.Fatal("the registering manager lost its transport")
	}
	if second.GetApprovalTransport(work, "phone") != nil {
		t.Fatal("another manager sees the transport")
	}
	personal := homeCtx(filepath.Join(tmp, "profiles", "personal"))
	if first.GetApprovalTransport(personal, "phone") != nil {
		t.Fatal("another profile sees the transport")
	}
}

// Asker standing in for the builtin prompt, failing the test when it has no scripted answer
type atAsker struct {
	t      *testing.T
	answer string
	calls  atomic.Int32
}

func (a *atAsker) Ask(context.Context, []tools.Question) (tools.FormResult, error) {
	if a.answer == "" {
		a.t.Fatalf("builtin prompt must not materialize")
	}
	a.calls.Add(1)
	return tools.FormResult{Outcome: "submitted", Answers: []tools.Answer{{Status: "answered", Text: a.answer}}}, nil
}

// Fresh manager registry, empty bundled table and the real transport resolver for a guard test
func atGuardSetup(t *testing.T) (context.Context, string, *Manager) {
	t.Helper()
	ledgerSwapBundled(t)
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	tools.RegisterApprovalTransportResolver(resolveRegisteredApprovalTransport)
	t.Cleanup(func() { tools.RegisterApprovalTransportResolver(resolveRegisteredApprovalTransport) })
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_GATEWAY_SESSION", "")
	t.Setenv("ICEBATH_INTERACTIVE", "")
	ctx := homeCtx(h)
	return ctx, h, GetPluginManager(ctx)
}

// Manual mode env selecting the phone transport with the given session markers
func atGuardEnv(h, sessionKey, fallback string, markers map[string]any) *tools.Env {
	approval := map[string]any{"transport": "phone"}
	if fallback != "" {
		approval["transport_fallback"] = fallback
	}
	env := &tools.Env{Home: h, Config: config.Tree{"approvals": map[string]any{"mode": "manual"}, "security": map[string]any{"approval": approval}}, Values: &sync.Map{}}
	env.Values.Store("session.key", sessionKey)
	for k, v := range markers {
		env.Values.Store("session."+k, v)
	}
	return env
}

func TestCliSelectedTransportReplacesBuiltinPrompt(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	var mu sync.Mutex
	var seen []tools.ApprovalRequest
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRecording(&mu, &seen, func(r tools.ApprovalRequest) tools.ApprovalTransportDecision { return r.Respond("once") })); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-cli-session", "", map[string]any{"interactive": true})
	env.Ask = &atAsker{t: t}
	result := tools.CheckAllCommandGuards(ctx, env, "rm -rf /tmp/example", "local", false)
	if !result.Approved || !result.UserApproved {
		t.Fatalf("%+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Command != "rm -rf /tmp/example" || strings.Join(seen[0].AllowedChoices, ",") != "once,session,always,deny" {
		t.Fatalf("seen %+v", seen)
	}
}

func TestGatewaySelectedTransportDoesNotRequireGatewayNotifier(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	var mu sync.Mutex
	var seen []tools.ApprovalRequest
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRecording(&mu, &seen, func(r tools.ApprovalRequest) tools.ApprovalTransportDecision { return r.Respond("once") })); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-gateway-session", "", map[string]any{"gateway": true})
	result := tools.CheckAllCommandGuards(ctx, env, "rm -rf /tmp/example", "local", false)
	if !result.Approved {
		t.Fatalf("%+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Surface != "gateway" {
		t.Fatalf("seen %+v", seen)
	}
}

func TestExecuteCodeGatewayUsesSelectedTransport(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	var mu sync.Mutex
	var seen []tools.ApprovalRequest
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRecording(&mu, &seen, func(r tools.ApprovalRequest) tools.ApprovalTransportDecision { return r.Respond("once") })); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-execute-code-session", "", map[string]any{"gateway": true})
	result := tools.CheckExecuteCodeGuard(ctx, env, "print('ok')", "local", false)
	if !result.Approved {
		t.Fatalf("%+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].PatternKey != "execute_code" || seen[0].Surface != "gateway" {
		t.Fatalf("seen %+v", seen)
	}
}

func TestTransportFailureDeniesWithoutBuiltinFallback(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	uncorrelated := func(context.Context, tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
		return tools.ApprovalTransportDecision{Choice: "once"}, nil
	}
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", uncorrelated); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-failure-session", "", map[string]any{"interactive": true})
	asker := &atAsker{t: t, answer: "once"}
	env.Ask = asker
	result := tools.CheckAllCommandGuards(ctx, env, "rm -rf /tmp/example", "local", false)
	if result.Approved || result.Outcome != "transport_invalid" || asker.calls.Load() != 0 {
		t.Fatalf("%+v builtin calls %d", result, asker.calls.Load())
	}
}

func TestExplicitBuiltinFallbackUsesExistingSurface(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	uncorrelated := func(context.Context, tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
		return tools.ApprovalTransportDecision{Choice: "once"}, nil
	}
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", uncorrelated); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-fallback-session", "builtin", map[string]any{"interactive": true})
	asker := &atAsker{t: t, answer: "once"}
	env.Ask = asker
	result := tools.CheckAllCommandGuards(ctx, env, "rm -rf /tmp/example", "local", false)
	if !result.Approved || asker.calls.Load() != 1 {
		t.Fatalf("%+v builtin calls %d", result, asker.calls.Load())
	}
}

func TestHardlineBlocksBeforeSelectedTransport(t *testing.T) {
	ctx, h, m := atGuardSetup(t)
	var mu sync.Mutex
	var seen []tools.ApprovalRequest
	if err := atContext(m, "fixture-approval").RegisterApprovalTransport(ctx, "phone", atRecording(&mu, &seen, func(r tools.ApprovalRequest) tools.ApprovalTransportDecision { return r.Respond("once") })); err != nil {
		t.Fatal(err)
	}
	env := atGuardEnv(h, "at-hardline-session", "", map[string]any{"interactive": true})
	result := tools.CheckAllCommandGuards(ctx, env, "rm -rf /", "local", false)
	mu.Lock()
	defer mu.Unlock()
	if result.Approved || len(seen) != 0 {
		t.Fatalf("%+v seen %d", result, len(seen))
	}
}

// Compiled-in twin of the hermes fixture-approval plugin recording every presentation into the home
func atFixturePlugin() Plugin {
	return ledgerBundled{m: Manifest{Name: "fixture-approval", Version: "1.0.0", Description: "approval transport fixture"}, register: func(ctx *Context) error {
		return ctx.RegisterApprovalTransport(context.Background(), "fixture", func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
			line, err := json.Marshal(map[string]any{"request_id": req.RequestID, "digest": req.Digest, "command": req.Command,
				"surface": req.Surface, "timeout_seconds": req.TimeoutSeconds})
			if err != nil {
				return tools.ApprovalTransportDecision{}, err
			}
			f, err := os.OpenFile(filepath.Join(home.Dir(context.Background()), "transport-invocations.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return tools.ApprovalTransportDecision{}, err
			}
			defer f.Close()
			if _, err := f.Write(append(line, '\n')); err != nil {
				return tools.ApprovalTransportDecision{}, err
			}
			return req.Respond("once"), nil
		})
	}}
}

func TestLiveTempHomeFixturePluginRoutesAndHardlineStaysCoreOwned(t *testing.T) {
	ledgerSwapBundled(t, atFixturePlugin())
	ResetPluginManagersForTests()
	t.Cleanup(ResetPluginManagersForTests)
	tools.RegisterApprovalTransportResolver(resolveRegisteredApprovalTransport)
	h := filepath.Join(t.TempDir(), "icebath-home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [fixture-approval]\napprovals:\n  mode: manual\n  timeout: 2\nsecurity:\n  approval:\n    transport: fixture\n")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_GATEWAY_SESSION", "")
	t.Setenv("ICEBATH_INTERACTIVE", "")
	ctx := homeCtx(h)
	manager := GetPluginManager(ctx)
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := func(markers map[string]any) *tools.Env {
		e := &tools.Env{Home: h, Config: cfg, Values: &sync.Map{}}
		e.Values.Store("session.key", "local")
		for k, v := range markers {
			e.Values.Store("session."+k, v)
		}
		return e
	}
	tools.ClearSession("local")
	cli := env(map[string]any{"interactive": true})
	routed := tools.CheckAllCommandGuards(ctx, cli, "rm -rf /tmp/icebath-approval-transport-fixture", "local", false)
	manager.DiscoverAndLoad(ctx, true)
	reloaded := tools.CheckAllCommandGuards(ctx, cli, "rm -rf /tmp/icebath-approval-transport-fixture-reloaded", "local", false)
	t.Setenv("ICEBATH_GATEWAY_SESSION", "1")
	gatewayRouted := tools.CheckAllCommandGuards(ctx, env(map[string]any{"gateway": true}), "rm -rf /tmp/icebath-approval-transport-fixture-gateway", "local", false)
	t.Setenv("ICEBATH_GATEWAY_SESSION", "")
	hardline := tools.CheckAllCommandGuards(ctx, cli, "rm -rf /", "local", false)
	raw, err := os.ReadFile(filepath.Join(h, "transport-invocations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	if !routed.Approved || !reloaded.Approved || !gatewayRouted.Approved {
		t.Fatalf("routed %+v reloaded %+v gateway %+v", routed, reloaded, gatewayRouted)
	}
	if len(records) != 3 || records[0]["request_id"] == "" || records[0]["digest"] == "" || records[0]["surface"] != "cli" || records[0]["timeout_seconds"] != 2.0 || records[2]["surface"] != "gateway" {
		t.Fatalf("records %v", records)
	}
	if hardline.Approved {
		t.Fatalf("hardline %+v", hardline)
	}
}
