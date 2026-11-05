package mcp

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Writes a profile config file holding body
func writeProfileConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Swaps the discovery pass for fn until the test ends
func swapDiscoveryPass(t *testing.T, fn func(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) error) {
	prev := discoveryPass
	discoveryPass = fn
	t.Cleanup(func() { discoveryPass = prev })
}

func TestMultiplexOwnerReloadReregistersProfilesThatAdoptedItsConnection(t *testing.T) {
	enter, env, homes := twoProfiles(t)
	cfg := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer shared"}}
	writeProfileConfig(t, homes["b"], "mcp_servers:\n  x:\n    url: https://mcp.example/x\n    headers:\n      Authorization: Bearer shared\n")
	ctxA := enter("a")
	scopeA := mcpRegistryScope(ctxA)
	srvA := multiplexServer(ctxA, env, "x", maps1(cfg))
	adoptAndRegister(t, ctxA, "x", srvA, cfg)
	ctxB := enter("b")
	if n, err := registerConnectedIntoCurrentScope(ctxB, env, map[string]*Rendered{"x": {Config: maps1(cfg)}}); n != 1 || err != nil {
		t.Fatalf("B adoption %d %v", n, err)
	}
	if got := toolsetNames(ctxB, "mcp-x"); !slices.Equal(got, []string{"mcp__x__t"}) {
		t.Fatalf("B toolset after adoption %v", got)
	}
	ShutdownMCPServers(ctxA, &scopeA, nil, 5*time.Second)
	for _, name := range srvA.registeredToolNames() {
		deregisterMCPToolAllScopes(ctxA, srvA, name)
	}
	mcpMu.Lock()
	for key, live := range mcpServers {
		if live == srvA {
			delete(mcpServers, key)
			delete(serverScopeKeys, key)
			delete(serverToolScopes, key)
		}
	}
	mcpMu.Unlock()
	if got := toolsetNames(ctxB, "mcp-x"); len(got) != 0 {
		t.Fatalf("the owner teardown left B's overlay %v", got)
	}
	swapDiscoveryPass(t, func(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) error {
		for name, r := range fresh {
			adoptAndRegister(t, ctx, name, multiplexServer(ctx, env, name, r.Config), r.Config)
		}
		return nil
	})
	registerMCPServers(enter("a"), env, map[string]*Rendered{"x": {Config: maps1(cfg)}})
	ctxB = enter("b")
	if got := toolsetNames(ctxB, "mcp-x"); !slices.Equal(got, []string{"mcp__x__t"}) {
		t.Fatalf("B lost its tools across the owner reload %v", got)
	}
	if st := Status(ctxB, env, map[string]*Rendered{"x": {Config: maps1(cfg)}}, true); st[0].Status != "connected" {
		t.Fatalf("B status %q", st[0].Status)
	}
}

func TestMultiplexServedProfileCheckFnVerdictDoesNotShadowLaunchProfile(t *testing.T) {
	enter, env, homes := twoProfiles(t)
	tools.SetMultiplexActive(false)
	cfgA := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer A"}}
	launch := context.Background()
	srvA := multiplexServer(launch, env, "x", cfgA)
	adoptAndRegister(t, launch, "x", srvA, cfgA)
	t.Cleanup(func() {
		_ = tools.Default.Deregister("mcp__x__t", "", "")
		tools.InvalidateCheckCache()
	})
	entry := tools.Default.Lookup(launch, "", "mcp__x__t")
	if entry == nil || entry.Check == nil {
		t.Fatal("the launch profile's tool must be registered with a check")
	}
	tools.InvalidateCheckCache()
	ctxB := enter("b")
	envB := &tools.Env{Home: homes["b"]}
	if !servesRoutedProfile(ctxB) || tools.ScopeKey(envB.Home) == tools.ScopeKey(env.Home) {
		t.Fatal("B's availability verdicts must be cached under B's own profile key")
	}
	want := map[string]bool{"mcp__x__t": true}
	if got := tools.Default.Definitions(ctxB, envB, want, true); len(got) != 0 {
		t.Fatalf("B sees the launch profile's connection %v", got)
	}
	if v, cached := tools.CachedCheckResult(envB, entry.Check); !cached || v {
		t.Fatalf("B verdict %v cached %v", v, cached)
	}
	if got := tools.Default.Definitions(launch, env, want, true); len(got) != 1 {
		t.Fatalf("B's verdict shadowed the launch profile %v", got)
	}
	if v, cached := tools.CachedCheckResult(env, entry.Check); !cached || !v {
		t.Fatalf("launch verdict %v cached %v", v, cached)
	}
}

func TestMultiplexLaunchProfilePruningAServerKeepsServedProfilesSameNamedConnection(t *testing.T) {
	enter, env, homes := twoProfiles(t)
	tools.SetMultiplexActive(false)
	cfg := map[string]any{"url": "https://mcp.example/x", "headers": map[string]any{"Authorization": "Bearer shared"}}
	ctxB := enter("b")
	scopeB := tools.ScopeKey(homes["b"])
	keyB, keyLaunch := ServerKey{Scope: scopeB, Name: "x"}, ServerKey{Name: "x"}
	srvB := multiplexServer(ctxB, env, "x", maps1(cfg))
	adoptServer(ctxB, "x", srvB)
	mcpMu.Lock()
	ownerB := serverScopeKeys[keyB]
	mcpMu.Unlock()
	if ownerB != scopeB {
		t.Fatalf("B owner %q", ownerB)
	}
	launch := context.Background()
	if s := mcpRegistryScope(launch); s != "" {
		t.Fatalf("launch registry scope %q", s)
	}
	srvLaunch := multiplexServer(launch, env, "x", maps1(cfg))
	adoptServer(launch, "x", srvLaunch)
	mcpMu.Lock()
	ownerLaunch, owned := serverScopeKeys[keyLaunch]
	serverToolScopes[keyLaunch] = map[string]bool{"c": true}
	mcpMu.Unlock()
	if !owned || ownerLaunch != "" {
		t.Fatalf("launch owner %q recorded %v", ownerLaunch, owned)
	}
	ensureMCPLoop()
	ShutdownMCPServers(launch, nil, []string{"x"}, 5*time.Second)
	mcpMu.Lock()
	_, launchLive := mcpServers[keyLaunch]
	_, launchOwned := serverScopeKeys[keyLaunch]
	liveB, ownerB := mcpServers[keyB], serverScopeKeys[keyB]
	adopters := map[string]map[string]bool{}
	for k, v := range orphanedAdopters {
		adopters[k] = map[string]bool{}
		for n := range v {
			adopters[k][n] = true
		}
	}
	mcpMu.Unlock()
	if launchLive || launchOwned {
		t.Fatalf("the launch profile's x survived pruning live %v owned %v", launchLive, launchOwned)
	}
	if liveB != srvB || ownerB != scopeB {
		t.Fatalf("pruning tore down B's x live %v owner %q", liveB == srvB, ownerB)
	}
	if !srvLaunch.shutdownEv.IsSet() || srvB.shutdownEv.IsSet() {
		t.Fatalf("closed launch %v B %v", srvLaunch.shutdownEv.IsSet(), srvB.shutdownEv.IsSet())
	}
	if !reflect.DeepEqual(adopters, map[string]map[string]bool{"c": {"x": true}}) {
		t.Fatalf("orphaned adopters %v", adopters)
	}
}

func TestMultiplexAdopterScopeSetupFailureLeaksNoOverrideAndContinues(t *testing.T) {
	_, env, _ := twoProfiles(t)
	root := t.TempDir()
	bad, good := filepath.Join(root, "bad-home"), filepath.Join(root, "good-home")
	for _, h := range []string{bad, good} {
		writeProfileConfig(t, h, "mcp_servers:\n  x:\n    url: https://mcp.example/x\n")
	}
	mcpMu.Lock()
	orphanedAdopters[bad] = map[string]bool{"x": true}
	orphanedAdopters[good] = map[string]bool{"x": true}
	mcpMu.Unlock()
	prevScope := adopterSecretScope
	adopterSecretScope = func(h string) map[string]string {
		if strings.Contains(h, "bad-home") {
			panic("corrupt profile home")
		}
		return map[string]string{}
	}
	t.Cleanup(func() { adopterSecretScope = prevScope })
	var registered []map[string]any
	var scopedHomes []string
	swapDiscoveryPass(t, func(ctx context.Context, env *tools.Env, fresh map[string]*Rendered) error {
		servers := map[string]any{}
		for name, r := range fresh {
			servers[name] = r.Config
		}
		registered = append(registered, servers)
		scopedHomes = append(scopedHomes, currentSecretScopeHome(ctx), home.Override(ctx))
		return nil
	})
	ctx := context.Background()
	reregisterOrphanedAdopters(ctx, env)
	if home.Override(ctx) != "" || currentSecretScopeHome(ctx) != "" {
		t.Fatal("an adopter's home or secret scope leaked into the caller")
	}
	if want := []map[string]any{{"x": map[string]any{"url": "https://mcp.example/x"}}}; !reflect.DeepEqual(registered, want) {
		t.Fatalf("registered %v", registered)
	}
	if !slices.Equal(scopedHomes, []string{good, good}) {
		t.Fatalf("the good adopter ran under %v", scopedHomes)
	}
	mcpMu.Lock()
	left := len(orphanedAdopters)
	mcpMu.Unlock()
	if left != 0 {
		t.Fatalf("pending adopters %d", left)
	}
}

func TestCheckFnVerdictOfOneServerDoesNotShadowAnotherInTheSameProfile(t *testing.T) {
	ctx, env := refreshEnv(t)
	live := regServer(env, "live", capsOf(false, false), &Tool{Name: "t"})
	adoptServer(ctx, "live", live)
	mustRegister(t, ctx, live, map[string]any{})
	gone := regServer(env, "gone", capsOf(false, false), &Tool{Name: "t"})
	mustRegister(t, ctx, gone, map[string]any{})
	tools.InvalidateCheckCache()
	t.Cleanup(tools.InvalidateCheckCache)
	both := map[string]bool{"mcp__gone__t": true, "mcp__live__t": true}
	var got []string
	for _, d := range tools.Default.Definitions(ctx, env, both, true) {
		got = append(got, d["function"].(map[string]any)["name"].(string))
	}
	if !slices.Equal(got, []string{"mcp__live__t"}) {
		t.Fatalf("one pass over both servers exposed %v", got)
	}
	if got := tools.Default.Definitions(ctx, env, map[string]bool{"mcp__live__t": true}, true); len(got) != 1 {
		t.Fatal("the disconnected server's cached verdict hid the connected server")
	}
	if got := tools.Default.Definitions(ctx, env, map[string]bool{"mcp__gone__t": true}, true); len(got) != 0 {
		t.Fatal("the connected server's cached verdict exposed the disconnected server")
	}
}
