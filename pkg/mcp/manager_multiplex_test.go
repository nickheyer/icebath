package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Multiplex on with clean ledgers, returning a context switcher for profile homes a and b
func twoProfiles(t *testing.T) (func(which string) context.Context, *tools.Env, map[string]string) {
	t.Helper()
	_, env := managerHome(t)
	multiplexOn(t)
	root := t.TempDir()
	homes := map[string]string{}
	for _, k := range []string{"a", "b"} {
		homes[k] = filepath.Join(root, "profiles", k)
		if err := os.MkdirAll(homes[k], 0o700); err != nil {
			t.Fatal(err)
		}
	}
	isolateMCPRegistry(t, context.Background())
	clearLedgers := func() {
		mcpMu.Lock()
		defer mcpMu.Unlock()
		clear(mcpServers)
		clear(serverScopeKeys)
		clear(serverToolScopes)
		clear(serverConnecting)
		clear(serverConnectErrors)
		clear(serverConnectRetryAfter)
		clear(serverConnectFailures)
		clear(serverErrorCounts)
		clear(serverBreakerOpenedAt)
		clear(lazyServerConfigs)
		clear(mcpToolServerNames)
		clear(orphanedAdopters)
		clear(parallelSafeServers)
		clear(serverTrustLevels)
		clear(toolReadOnlyHints)
	}
	clearLedgers()
	t.Cleanup(func() {
		for _, h := range homes {
			scope := tools.ScopeKey(h)
			for _, toolset := range []string{"mcp-x", "mcp-s"} {
				for _, name := range tools.Default.ToolNamesForToolset(context.Background(), scope, toolset) {
					_ = tools.Default.Deregister(name, scope, "")
				}
			}
		}
		clearLedgers()
	})
	return func(which string) context.Context {
		return home.WithOverride(context.Background(), homes[which])
	}, env, homes
}

// Live server task with one tool whose identity digest resolves in ctx
func multiplexServer(ctx context.Context, env *tools.Env, name string, cfg map[string]any) *serverTask {
	s := regServer(env, name, capsOf(false, false), &Tool{Name: "t", Description: "d", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}})
	s.config = cfg
	s.toolTimeout = 30
	if digest, ok := adopterIdentityDigest(ctx, env, name, &Rendered{Config: cfg}); ok {
		s.resolvedIdentity = digest
	}
	return s
}

// Adopts s under ctx and registers its tools there
func adoptAndRegister(t *testing.T, ctx context.Context, name string, s *serverTask, cfg map[string]any) {
	t.Helper()
	adoptServer(ctx, name, s)
	names, err := registerServerTools(ctx, name, s, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.toolNames = names
	s.mu.Unlock()
}

// Tool names the profile of ctx sees in toolset
func toolsetNames(ctx context.Context, toolset string) []string {
	return tools.Default.ToolNamesForToolset(ctx, mcpRegistryScope(ctx), toolset)
}

func TestMultiplexSameNamedServerWithOtherCredentialsIsASeparateConnection(t *testing.T) {
	enter, env, homes := twoProfiles(t)
	cfgA := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer A"}}
	cfgB := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer B"}}
	ctxA := enter("a")
	srvA := multiplexServer(ctxA, env, "x", cfgA)
	adoptAndRegister(t, ctxA, "x", srvA, cfgA)
	if got := toolsetNames(ctxA, "mcp-x"); !slices.Equal(got, []string{"mcp__x__t"}) {
		t.Fatalf("A toolset %v", got)
	}
	for range circuitBreakerThreshold {
		bumpServerError(ctxA, "x", false)
	}
	noteConnectFailure(ctxA, "y", errors.New("boom"))
	ctxB := enter("b")
	if got := toolsetNames(ctxB, "mcp-x"); len(got) != 0 {
		t.Fatalf("B sees A's tools %v", got)
	}
	if st := Status(ctxB, env, map[string]*Rendered{"x": {Config: cfgB}}, true); st[0].Status != "configured" {
		t.Fatalf("B status %q", st[0].Status)
	}
	if _, ok := selectNewServers(ctxB, map[string]*Rendered{"x": {Config: cfgB}})["x"]; !ok {
		t.Fatal("B's differently authenticated x must be a connect candidate")
	}
	if connectCooldownActive(ctxB, "y") || checkCircuitBreaker(ctxB, "x") != "" {
		t.Fatal("A's cooldown and breaker leaked into B")
	}
	scopeA := tools.ScopeKey(homes["a"])
	same := map[string]any{"url": "https://mcp.example/same", "headers": map[string]any{"Authorization": "Bearer same"}}
	for _, name := range []string{"ok", "boom"} {
		s := multiplexServer(ctxA, env, name, maps1(same))
		s.tools = nil
		mcpMu.Lock()
		mcpServers[ServerKey{Scope: scopeA, Name: name}] = s
		serverToolScopes[ServerKey{Scope: scopeA, Name: name}] = map[string]bool{scopeA: true}
		mcpMu.Unlock()
	}
	prev := connectInputs
	connectInputs = func(ctx context.Context, env *tools.Env, name string, r *Rendered) ([]any, map[string]bool, error) {
		if name == "boom" {
			return nil, nil, errors.New("secret backend exploded")
		}
		return prev(ctx, env, name, r)
	}
	scopeB := mcpRegistryScope(ctxB)
	_, err := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"ok": {Config: maps1(same)}, "boom": {Config: maps1(same)}})
	connectInputs = prev
	if err != nil {
		t.Fatal(err)
	}
	mcpMu.Lock()
	okAdopted, boomAdopted := serverToolScopes[ServerKey{Scope: scopeA, Name: "ok"}][scopeB], serverToolScopes[ServerKey{Scope: scopeA, Name: "boom"}][scopeB]
	mcpMu.Unlock()
	if !okAdopted || boomAdopted {
		t.Fatalf("ok adopted %v, boom adopted %v", okAdopted, boomAdopted)
	}
	tagSecretSource(t)
	for _, h := range homes {
		writeEnvFile(t, h, scopeTokenName+"=tok\n")
	}
	cfgS := map[string]any{"command": mockBinary(t), "args": []any{}}
	ctxA = enter("a")
	srvS := multiplexServer(installOwnerSecretScope(ctxA), env, "s", cfgS)
	if srvS.resolvedIdentity == "" {
		t.Fatal("the owner's stdio identity must resolve")
	}
	adoptAndRegister(t, ctxA, "s", srvS, cfgS)
	ctxB = enter("b")
	if currentSecretScopeHome(ctxB) != "" {
		t.Fatal("B reconciles with no ambient secret scope")
	}
	if n, err := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"s": {Config: maps1(cfgS)}}); n != 0 || err != nil {
		t.Fatalf("B adopted A's stdio child %d %v", n, err)
	}
	if _, ok := selectNewServers(ctxB, map[string]*Rendered{"s": {Config: maps1(cfgS)}})["s"]; !ok {
		t.Fatal("B must get its own stdio connection")
	}
}

func TestMultiplexOAuthServerIsNotAdoptedAcrossProfiles(t *testing.T) {
	enter, env, _ := twoProfiles(t)
	cfg := map[string]any{"url": "https://mcp.example/x", "auth": "oauth"}
	ctxA := enter("a")
	srvA := multiplexServer(ctxA, env, "x", cfg)
	adoptAndRegister(t, ctxA, "x", srvA, cfg)
	if n, _ := registerConnectedIntoCurrentScope(ctxA, env, map[string]*Rendered{"x": {Config: maps1(cfg)}}); n != 0 {
		t.Fatalf("A healed %d", n)
	}
	if got := toolsetNames(ctxA, "mcp-x"); !slices.Equal(got, []string{"mcp__x__t"}) {
		t.Fatalf("A toolset %v", got)
	}
	ctxB := enter("b")
	if n, _ := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"x": {Config: maps1(cfg)}}); n != 0 {
		t.Fatalf("B adopted the OAuth connection %d", n)
	}
	if got := toolsetNames(ctxB, "mcp-x"); len(got) != 0 {
		t.Fatalf("B toolset %v", got)
	}
	var connected []*serverTask
	prev := discoveryPass
	discoveryPass = func(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) error {
		for name, r := range fresh {
			s := multiplexServer(ctx, env, name, r.Config)
			connected = append(connected, s)
			adoptServer(ctx, name, s)
		}
		return nil
	}
	t.Cleanup(func() { discoveryPass = prev })
	registerMCPServers(ctxB, env, map[string]*Rendered{"x": {Config: maps1(cfg)}})
	mcpMu.Lock()
	ownB, ownA := mcpServers[ServerKey{Scope: mcpRegistryScope(ctxB), Name: "x"}], mcpServers[ServerKey{Scope: mcpRegistryScope(ctxA), Name: "x"}]
	mcpMu.Unlock()
	if len(connected) == 0 || ownB != connected[0] || ownA != srvA {
		t.Fatalf("connected %d, B owns its own %v, A kept its own %v", len(connected), ownB != nil && len(connected) > 0 && ownB == connected[0], ownA == srvA)
	}
}

func TestMultiplexSameNamedServerWithOtherMTLSIdentityIsASeparateConnection(t *testing.T) {
	enter, env, _ := twoProfiles(t)
	cfgA := map[string]any{"url": "https://mcp.example/x", "client_cert": "/certs/profile-a.pem", "client_key": "/certs/profile-a.key"}
	cfgB := map[string]any{"url": "https://mcp.example/x", "client_cert": "/certs/profile-b.pem", "client_key": "/certs/profile-b.key"}
	ctxA := enter("a")
	adoptAndRegister(t, ctxA, "x", multiplexServer(ctxA, env, "x", cfgA), cfgA)
	ctxB := enter("b")
	if _, err := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"x": {Config: cfgB}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := selectNewServers(ctxB, map[string]*Rendered{"x": {Config: cfgB}})["x"]; !ok {
		t.Fatal("B's other client certificate must be its own connection")
	}
}

// Shallow copy of a config map
func maps1(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func TestMultiplexUntrustedAdopterOfAFullProfilesConnectionKeepsItsOwnTrustGate(t *testing.T) {
	enter, env, _ := twoProfiles(t)
	route := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer shared"}}
	cfgA, cfgB := maps1(route), maps1(route)
	cfgA["trust"], cfgB["trust"] = "full", "untrusted"
	var asked []string
	prev := requestTrustConsent
	requestTrustConsent = func(_ context.Context, _ *tools.Env, message, _ string, _ tools.ElicitationOptions) string {
		asked = append(asked, message)
		return "deny"
	}
	t.Cleanup(func() { requestTrustConsent = prev })
	ctxA := enter("a")
	adoptAndRegister(t, ctxA, "x", multiplexServer(ctxA, env, "x", cfgA), cfgA)
	ctxB := enter("b")
	if n, err := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"x": {Config: cfgB}}); n != 1 || err != nil {
		t.Fatalf("B healed %d %v", n, err)
	}
	if trustGateCheck(ctxB, env, "x", "t", nil, map[string]any{}, "{}") == "" || len(asked) == 0 {
		t.Fatalf("B's untrusted policy must gate the call, asked %v", asked)
	}
	if got := trustGateCheck(enter("a"), env, "x", "t", nil, map[string]any{}, "{}"); got != "" || len(asked) != 1 {
		t.Fatalf("A's full trust must pass without asking, got %q asked %d", got, len(asked))
	}
}
