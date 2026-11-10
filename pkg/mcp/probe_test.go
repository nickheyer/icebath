package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// probeHome writes servers into an isolated config.yaml and returns the matching env
func probeHome(t *testing.T, servers map[string]any) *tools.Env {
	t.Helper()
	h := isolateHome(t)
	resetLedgers(t)
	if err := config.SaveUser(context.Background(), h, map[string]any{"mcp_servers": servers}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ShutdownMCPServers(context.Background(), nil, nil, 10*time.Second) })
	return &tools.Env{Home: h}
}

// test_mcp_probe.py TestProbeMcpServerTools test_returns_empty_when_mcp_not_available
func TestProbeMcpServerToolsReturnsEmptyWhenMcpNotAvailable(t *testing.T) {
	env := probeHome(t, map[string]any{"github": map[string]any{"command": mockBinary(t), "connect_timeout": 5}})
	closeSDKGate(t)
	if got := Probe(context.Background(), env); len(got) != 0 || serverSpawned("github") {
		t.Fatalf("got %v spawned %v", got, serverSpawned("github"))
	}
}

func TestProbeMcpServerToolsFailedServerOmittedFromResults(t *testing.T) {
	env := probeHome(t, map[string]any{
		"github": map[string]any{"command": mockBinary(t), "connect_timeout": 5},
		"broken": map[string]any{"command": filepath.Join(t.TempDir(), "nonexistent"), "connect_timeout": 5},
	})
	got := Probe(context.Background(), env)
	if _, ok := got["broken"]; ok {
		t.Fatalf("broken server listed %v", got)
	}
	if len(got["github"]) == 0 || got["github"][0].Name == "" {
		t.Fatalf("github tools missing %v", got)
	}
	mcpMu.Lock()
	live := len(mcpServers)
	mcpMu.Unlock()
	if live != 0 {
		t.Fatalf("probe left %d servers registered", live)
	}
}

func TestProbeMcpServerToolsSkipsDisabledServers(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "disabled.log")
	env := probeHome(t, map[string]any{
		"github":       map[string]any{"command": mockBinary(t), "connect_timeout": 5},
		"disabled_one": map[string]any{"command": mockBinary(t), "enabled": false, "env": map[string]any{"MOCK_MCP_RECORD": rec}},
	})
	got := Probe(context.Background(), env)
	if _, ok := got["disabled_one"]; ok || len(got["github"]) == 0 {
		t.Fatalf("got %v", got)
	}
	if data, err := os.ReadFile(rec); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("disabled server was spawned and saw %q", data)
	}
}
