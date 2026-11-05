package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSessionMCPRegistryIsolationAndScopedRelease(t *testing.T) {
	_, env := managerHome(t)
	bare := context.Background()
	isolateMCPRegistry(t, bare)
	scopeA, scopeB := "editor-session:a", "editor-session:b"
	t.Cleanup(func() {
		ShutdownMCPServers(bare, &scopeA, nil, 5*time.Second)
		ShutdownMCPServers(bare, &scopeB, nil, 5*time.Second)
	})
	dispatch := func(ctx context.Context, name string) string {
		d := tools.Default.Lookup(ctx, mcpRegistryScope(ctx), name)
		if d == nil {
			t.Errorf("%s not visible in its own scope", name)
			return ""
		}
		return d.Handler(ctx, &tools.Call{Name: name, Args: map[string]any{}})
	}
	type outcome struct{ name, value string }
	connect := func(scope, name, marker string) outcome {
		ctx := tools.WithSessionToolScope(bare, scope)
		r := mockEntry(t, map[string]any{"env": map[string]any{"MOCK_MCP_TOOLS": "env", "OWNED_MARKER": marker}, "connect_timeout": 5})
		if _, err := Connect(ctx, env, map[string]*Rendered{name: r}); err != nil {
			t.Errorf("connect %s: %v", name, err)
			return outcome{}
		}
		var names []string
		for _, d := range tools.Default.Snapshot(ctx, mcpRegistryScope(ctx)) {
			if strings.HasPrefix(d.Name, "mcp__") {
				names = append(names, d.Name)
			}
		}
		if len(names) != 1 {
			t.Errorf("scope %s sees %v", scope, names)
			return outcome{}
		}
		return outcome{names[0], dispatch(ctx, names[0])}
	}
	var a, b outcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a = connect(scopeA, "owned_a", "ONLY_A") }()
	go func() { defer wg.Done(); b = connect(scopeB, "owned_b", "ONLY_B") }()
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	if !strings.Contains(a.value, "ONLY_A") || strings.Contains(a.value, "ONLY_B") {
		t.Fatalf("session a dispatch %q", a.value)
	}
	if !strings.Contains(b.value, "ONLY_B") || strings.Contains(b.value, "ONLY_A") {
		t.Fatalf("session b dispatch %q", b.value)
	}
	if tools.Default.Lookup(bare, "", a.name) != nil || tools.Default.Lookup(bare, "", b.name) != nil {
		t.Fatal("session registrations leaked into the global registry")
	}
	ShutdownMCPServers(bare, &scopeA, nil, 5*time.Second)
	ctxB := tools.WithSessionToolScope(bare, scopeB)
	if tools.Default.Lookup(ctxB, mcpRegistryScope(ctxB), a.name) != nil {
		t.Fatal("session a tool visible to session b")
	}
	if got := dispatch(ctxB, b.name); !strings.Contains(got, "ONLY_B") {
		t.Fatalf("session b lost its server after a scoped release of a, got %q", got)
	}
}

// Removes global MCP tools other tests left registered so the session scopes start from a fresh registry
func isolateMCPRegistry(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, d := range tools.Default.Snapshot(ctx, "") {
		if strings.HasPrefix(d.Name, "mcp__") {
			if err := tools.Default.Deregister(d.Name, "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
}
