package acp

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tools"
)

func blockedDiscovery(t *testing.T, threadName string) (release func(), done <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	config.ResetCache()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("mcp_servers:\n  slow:\n    url: https://mcp.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	block, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(block) }) }
	previous := mcp.SwapDiscoveryPass(func(ctx context.Context, env *tools.Env, allowed []string) ([]string, error) {
		select {
		case <-block:
		case <-time.After(5 * time.Second):
		}
		close(finished)
		return nil, nil
	})
	ctx := context.Background()
	t.Cleanup(func() {
		release()
		timeout := 2 * time.Second
		mcp.JoinDiscovery(ctx, &timeout)
		mcp.SwapDiscoveryPass(previous)
	})
	env := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
	started := time.Now()
	mcp.StartBackgroundDiscovery(ctx, env, slog.New(slog.DiscardHandler), threadName)
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("StartBackgroundDiscovery blocked for %s", elapsed)
	}
	return release, finished
}

func discoverySession(t *testing.T, fake *mockAgent) (*HermesACPAgent, *SessionState) {
	t.Helper()
	noStore(t)
	m := NewSessionManager(func() any { return fake }, nil)
	return NewHermesACPAgent(m), mustCreate(t, m, ".")
}

func lateRefreshAgent() *mockAgent {
	return &mockAgent{model: "fake-model", provider: "fake-provider", enabledToolsets: []string{"icebath-acp"}, disabledToolsets: []string{},
		toolDefs: []map[string]any{}, validToolNames: map[string]bool{}}
}

type refreshLog struct {
	mu     sync.Mutex
	agents []any
}

func (r *refreshLog) add(a any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents = append(r.agents, a)
}

func (r *refreshLog) seen() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.agents...)
}

func TestAcpBackgroundDiscoveryDoesNotBlockStartup(t *testing.T) {
	release, _ := blockedDiscovery(t, "test-acp-discovery")
	if !mcp.DiscoveryInFlight(context.Background()) {
		t.Fatal("discovery is not in flight")
	}
	release()
	timeout := 2 * time.Second
	if !mcp.JoinDiscovery(context.Background(), &timeout) {
		t.Fatal("discovery did not finish")
	}
}

func TestAcpLateRefreshAddsToolsWhenDiscoveryLandsAfterBuild(t *testing.T) {
	release, done := blockedDiscovery(t, "test-acp-late")
	fake := lateRefreshAgent()
	a, st := discoverySession(t, fake)
	select {
	case <-done:
		t.Fatal("discovery finished too early for this test")
	default:
	}
	refreshed := &refreshLog{}
	swap(t, &refreshAgentMCPTools, func(ctx context.Context, ag any) map[string]bool {
		target := ag.(*mockAgent)
		target.SetToolSnapshot([]map[string]any{{"function": map[string]any{"name": "mcp_slow_tool"}}}, map[string]bool{"mcp_slow_tool": true})
		refreshed.add(ag)
		return map[string]bool{"mcp_slow_tool": true}
	})
	a.scheduleMCPLateRefresh(context.Background(), st)
	release()
	deadline := time.Now().Add(5 * time.Second)
	for len(refreshed.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	a.background.Wait()
	seen := refreshed.seen()
	if len(seen) == 0 {
		t.Fatal("late refresh did not rebuild the tool snapshot")
	}
	if seen[0] != any(fake) || !fake.validToolNames["mcp_slow_tool"] {
		t.Fatalf("refreshed %v names %v", seen, fake.validToolNames)
	}
}

func TestAcpLateRefreshSkipsAfterFirstTurn(t *testing.T) {
	release, _ := blockedDiscovery(t, "test-acp-cache")
	fake := lateRefreshAgent()
	fake.apiCallCount = 1
	a, st := discoverySession(t, fake)
	refreshed := &refreshLog{}
	swap(t, &refreshAgentMCPTools, func(ctx context.Context, ag any) map[string]bool {
		refreshed.add(ag)
		return map[string]bool{}
	})
	a.scheduleMCPLateRefresh(context.Background(), st)
	release()
	time.Sleep(500 * time.Millisecond)
	a.background.Wait()
	if len(refreshed.seen()) != 0 {
		t.Fatal("late refresh rebuilt tools after the first turn")
	}
}

func TestAcpLateRefreshSkipsWhileTurnRunning(t *testing.T) {
	release, _ := blockedDiscovery(t, "test-acp-running")
	fake := lateRefreshAgent()
	a, st := discoverySession(t, fake)
	st.IsRunning = true
	refreshed := &refreshLog{}
	swap(t, &refreshAgentMCPTools, func(ctx context.Context, ag any) map[string]bool {
		refreshed.add(ag)
		return map[string]bool{}
	})
	a.scheduleMCPLateRefresh(context.Background(), st)
	release()
	time.Sleep(500 * time.Millisecond)
	a.background.Wait()
	if len(refreshed.seen()) != 0 {
		t.Fatal("late refresh rebuilt tools while a turn was running")
	}
}
