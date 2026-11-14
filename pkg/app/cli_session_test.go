package app

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Runs the non quiet one shot entry against a recording conversation and returns the recorded calls
func oneShotCalls(t *testing.T, query string) []map[string]any {
	t.Helper()
	recordPrints(t)
	resetProcessState(t)
	t.Setenv("ICEBATH_KANBAN_GOAL_MODE", "")
	SetCLIConfig(map[string]any{"model": map[string]any{"default": "test/model", "provider": "openai", "base_url": "http://127.0.0.1:9/v1"},
		"agent": map[string]any{"max_turns": 5}, "display": map[string]any{}, "skills": map[string]any{"auto_load": []any{}}})
	t.Cleanup(func() { SetCLIConfig(nil) })
	var calls []map[string]any
	patchTurn(t, func(_ context.Context, _ cliAgent, _ any, o agent.RunConversationOptions) (map[string]any, error) {
		_, envSet := os.LookupEnv(provider.TurnAuthorEnvName)
		calls = append(calls, map[string]any{"turn_author": o.TurnAuthor, "env_author_set": envSet})
		return map[string]any{"final_response": "done", "completed": true}, nil
	})
	initTurnAgent = func(c *CLI, _ string, _ *cliRuntime, _ map[string]any) bool {
		if c.agentRef() == nil {
			c.setAgent(offlineAgent(t, c))
		}
		return true
	}
	Main(context.Background(), MainOptions{Query: query, Oneshot: true, Model: "test/model", Provider: "openai",
		APIKey: "test-key-1234567890", BaseURL: "http://127.0.0.1:9/v1", MaxTurns: ptr(5)})
	return calls
}

func TestDispatcherAuthorReachesTheTurnAndIsConsumed(t *testing.T) {
	t.Setenv(provider.TurnAuthorEnvName, `{"id": "bot:relay", "name": "relayed teammate", "is_bot": true}`)
	calls := oneShotCalls(t, "summarize the report")
	if len(calls) != 1 {
		t.Fatalf("turn calls %v", calls)
	}
	want := map[string]any{"id": "bot:relay", "name": "relayed teammate", "is_bot": true}
	if !reflect.DeepEqual(calls[0]["turn_author"], want) {
		t.Fatalf("turn author %#v", calls[0]["turn_author"])
	}
	if calls[0]["env_author_set"] != false {
		t.Fatal("tool subprocesses must not inherit the dispatcher's author")
	}
}

func TestHumanOneShotKeepsAnUnattributedTurn(t *testing.T) {
	t.Setenv(provider.TurnAuthorEnvName, "")
	_ = os.Unsetenv(provider.TurnAuthorEnvName)
	calls := oneShotCalls(t, "hello")
	if len(calls) != 1 {
		t.Fatalf("turn calls %v", calls)
	}
	if author, _ := calls[0]["turn_author"].(map[string]any); len(author) != 0 {
		t.Fatalf("a human one shot carried an author: %#v", calls[0]["turn_author"])
	}
}

func TestPrintExitSummaryNamesTheResumeCommandAndCounts(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	c.sessionID = "20261109_101010_abc123"
	c.sessionStart = time.Now().Add(-(time.Hour + 2*time.Minute + 3*time.Second))
	c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "1"}}},
		{"role": "tool", "content": "ok"}, {"role": "assistant", "content": "done"}}
	appendSessionEndMessages([]string{"plugin says bye"})
	c.printExitSummary(false)
	out := strings.Join(rec.raw(), "\n")
	for _, want := range []string{"plugin says bye", "Resume this session with:", "  icebath --resume 20261109_101010_abc123", "Session:        20261109_101010_abc123", "1h 2m 3s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary %q lacks %q", out, want)
		}
	}
	if len(takeSessionEndMessages()) != 0 {
		t.Fatal("the session end messages must be consumed")
	}
	rec.reset()
	c.conversationHistory = nil
	c.printExitSummary(false)
	if out := strings.Join(rec.raw(), "\n"); !strings.Contains(out, "Goodbye") || strings.Contains(out, "--resume") {
		t.Fatalf("an empty session printed %q", out)
	}
}
