package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// A home holding Bot Mode managed teammate profiles
func botModeManagedHome(t *testing.T, teammates ...string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	for _, name := range teammates {
		d := filepath.Join(h, "profiles", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "description: teammate for tests\nui_meta:\n  icebath-bots:\n    shape: cloud\n"
		if err := os.WriteFile(filepath.Join(d, "profile.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// An agent whose session row in the home's state.db carries the title
func botModeAgent(t *testing.T, h, title string) *AIAgent {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &AIAgent{sessionDB: db, sessionID: "sess-1", model: "m", platform: "cli", botModeProtocol: true,
		tools: []any{}, validToolNames: map[string]bool{}}
	if err := a.ensureDBSession(); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		if _, err := db.SetSessionTitle(context.Background(), "sess-1", title); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func injectedNames(a *AIAgent) []string {
	var names []string
	for _, t := range a.tools {
		names = append(names, t.(map[string]any)["function"].(map[string]any)["name"].(string))
	}
	return names
}

func TestInjectsOnlyIntoBotChatOnManagedInstall(t *testing.T) {
	a := botModeAgent(t, botModeManagedHome(t, "researcher"), "Bot Chat")
	if !ensureMessageAgentTool(context.Background(), a) {
		t.Fatal("managed Bot Chat agent refused the tool")
	}
	if got := injectedNames(a); !slices.Equal(got, []string{messageAgentToolName}) || !a.validToolNames[messageAgentToolName] {
		t.Fatalf("tools %v valid %v", got, a.validToolNames)
	}
	if !ensureMessageAgentTool(context.Background(), a) || len(a.tools) != 1 {
		t.Fatalf("second call changed the tool list %v", injectedNames(a))
	}
}

func TestRestoresAllowlistWhenSchemaSurvivesSurfaceRefresh(t *testing.T) {
	a := botModeAgent(t, botModeManagedHome(t, "researcher"), "Bot Chat")
	if !ensureMessageAgentTool(context.Background(), a) || len(a.tools) != 1 {
		t.Fatal("first injection")
	}
	a.validToolNames = map[string]bool{}
	if !ensureMessageAgentTool(context.Background(), a) || !a.validToolNames[messageAgentToolName] || len(a.tools) != 1 {
		t.Fatalf("allowlist %v tools %v", a.validToolNames, injectedNames(a))
	}
}

func TestNeverInjectsOutsideBotChat(t *testing.T) {
	for _, title := range []string{"", "My research chat", "Group: room-abc123", "handoff-12ab34cd"} {
		a := botModeAgent(t, botModeManagedHome(t, "researcher"), title)
		if ensureMessageAgentTool(context.Background(), a) || len(a.tools) != 0 || len(a.validToolNames) != 0 {
			t.Fatalf("title %q injected %v %v", title, injectedNames(a), a.validToolNames)
		}
	}
}

func TestNeverInjectsOnUnmanagedInstall(t *testing.T) {
	a := botModeAgent(t, botModeManagedHome(t), "Bot Chat")
	if ensureMessageAgentTool(context.Background(), a) || len(a.tools) != 0 {
		t.Fatalf("unmanaged install injected %v", injectedNames(a))
	}
}

func TestConfigToggleDisablesInjection(t *testing.T) {
	a := botModeAgent(t, botModeManagedHome(t, "researcher"), "Bot Chat")
	a.botModeProtocol = false
	if ensureMessageAgentTool(context.Background(), a) || len(a.tools) != 0 || messageAgentAuthorized(a) {
		t.Fatalf("protocol off injected %v", injectedNames(a))
	}
}

func TestSchemaNeverInGlobalRegistry(t *testing.T) {
	if tools.Default.Get(messageAgentToolName) != nil {
		t.Fatal("message_agent is registered")
	}
	for name, ts := range tools.Toolsets {
		if slices.Contains(ts.Tools, messageAgentToolName) {
			t.Fatalf("toolset %s carries message_agent", name)
		}
	}
	if messageAgentToolSchema()["function"].(map[string]any)["name"] != messageAgentToolName {
		t.Fatal("schema name")
	}
}
