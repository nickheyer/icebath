package tuiserver

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

// TUI session created through the RPC over the shared store, its live id and stored key
func submitRowSession(t *testing.T, db *state.SessionDB) (string, string) {
	t.Helper()
	useSharedDB(t, db)
	resp := lookupMethod("session.create")(context.Background(), "c", map[string]any{"cols": 96, "source": "tui"})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("create %v", resp)
	}
	return strOr(result["session_id"]), strOr(result["stored_session_id"])
}

// Agent owning the real flush on the stored session key, the crash persist at turn start runs the same code
func submitRowFlushAgent(t *testing.T, db *state.SessionDB, key string) *agent.AIAgent {
	t.Helper()
	h := t.TempDir()
	a, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{APIKey: "test-key-1234567890", BaseURL: "https://openrouter.ai/api/v1",
		Model: "test/model", SessionDB: db, SessionID: key, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
		EnabledToolsets: []string{}, Cwd: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
