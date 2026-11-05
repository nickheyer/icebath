package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers a tool under a toolset in the default registry for the life of the test
func registerForTest(t *testing.T, name, toolset string) {
	tools.Default.Remove(name)
	tools.Default.Register(testToolDef(name, toolset))
	t.Cleanup(func() { tools.Default.Remove(name) })
}

func TestHealthRefreshToolsSwapsRegistryInPlace(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	sess, _ := mockSession(t, map[string]string{"MOCK_MCP_PAGES": "3"}, SessionOptions{Name: "hsrv"})
	if _, err := sess.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	keep, gone, foreign := PrefixedToolName("hsrv", "tool0"), PrefixedToolName("hsrv", "gone"), PrefixedToolName("hsrv", "other")
	registerForTest(t, keep, "mcp-hsrv")
	registerForTest(t, gone, "mcp-hsrv")
	registerForTest(t, foreign, "mcp-elsewhere")
	for i := 1; i < 3; i++ {
		name := PrefixedToolName("hsrv", "tool"+string(rune('0'+i)))
		t.Cleanup(func() { tools.Default.Remove(name) })
	}
	s := &serverTask{name: "hsrv", session: sess, toolNames: []string{keep, gone, foreign}, config: map[string]any{"tools": map[string]any{"resources": false, "prompts": false}}}
	if err := s.refreshTools(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.registeredToolNames(), ","); got != "mcp__hsrv__tool0,mcp__hsrv__tool1,mcp__hsrv__tool2" {
		t.Fatalf("registered %s", got)
	}
	if tools.Default.Get(gone) != nil || tools.Default.Get(keep) == nil {
		t.Fatal("stale owned tool must go, listed tool must stay")
	}
	if d := tools.Default.Get(foreign); d == nil || d.Toolset != "mcp-elsewhere" {
		t.Fatal("a colliding name owned by another server is never deregistered")
	}
	if logs.count("MCP server 'hsrv': tools changed dynamically — added: mcp__hsrv__tool1, mcp__hsrv__tool2; removed: mcp__hsrv__gone, mcp__hsrv__other. Verify these changes are expected.") != 1 {
		t.Fatalf("change warning %q", logs.text())
	}
	if err := s.refreshTools(ctx); err != nil || logs.count("MCP server 'hsrv': dynamically refreshed 3 tool(s) (no changes)") != 1 {
		t.Fatalf("unchanged refresh %v %q", err, logs.text())
	}
}

func TestHealthRefreshToolsSkips(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	idle := &serverTask{name: "idle", toolNames: []string{"mcp__idle__a"}}
	if err := idle.refreshTools(ctx); err != nil || logs.count("MCP server 'idle': skipping dynamic tool refresh; session not connected") != 1 {
		t.Fatalf("disconnected refresh %v %q", err, logs.text())
	}
	if got := idle.registeredToolNames(); len(got) != 1 || idle.rpc.Locked() {
		t.Fatalf("skipped refresh changes nothing and releases the rpc lock: %v", got)
	}
	silent := &serverTask{name: "notools", initializeResult: &InitializeResult{Capabilities: &ServerCapabilities{}}}
	if err := silent.refreshTools(ctx); err != nil || logs.count("notools") != 0 {
		t.Fatalf("a server without the tools capability is never listed %v %q", err, logs.text())
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	busy := &serverTask{name: "busy"}
	if err := busy.rpc.Lock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := busy.refreshTools(cancelled); err == nil {
		t.Fatal("a refresh queued behind a held rpc lock ends with its context")
	}
	busy.rpc.Unlock()
}

// Minimal registrable tool definition
func testToolDef(name, toolset string) tools.Def {
	return tools.Def{Name: name, Toolset: toolset, Schema: map[string]any{"name": name},
		Handler: func(context.Context, *tools.Call) string { return "{}" }}
}
