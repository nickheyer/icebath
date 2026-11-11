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
	"github.com/nickheyer/icebath/pkg/state"
)

func TestCloseWaitsForAtomicCliStagingBeforeSnapshot(t *testing.T) {
	srv, _ := chatCompletionsServer(t, "old answer")
	c := liveChatCLI(t, srv.URL+"/v1")
	if _, ok := c.chat("old prompt", nil, false); !ok {
		t.Fatal("the first turn produced no response")
	}
	stagingEntered, releaseStaging := make(chan struct{}), make(chan struct{})
	runEntered, releaseRun := make(chan struct{}), make(chan struct{})
	prevHook, prevRun := stageAppendHook, runTurnConversation
	stageAppendHook = func() {
		close(stagingEntered)
		<-releaseStaging
	}
	prefix := append([]map[string]any{}, c.conversationHistory...)
	runTurnConversation = func(context.Context, *agent.AIAgent, any, agent.RunConversationOptions) (map[string]any, error) {
		close(runEntered)
		<-releaseRun
		return map[string]any{"final_response": "done", "messages": append(append([]map[string]any{}, prefix...), map[string]any{"role": "assistant", "content": "done"}),
			"api_calls": 1, "completed": true, "partial": true, "response_previewed": true}, nil
	}
	t.Cleanup(func() { stageAppendHook, runTurnConversation = prevHook, prevRun })
	chatDone := make(chan struct{})
	go func() {
		defer close(chatDone)
		c.chat("new prompt", nil, false)
	}()
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	wait(stagingEntered, "the staging append")
	closeFinished := make(chan struct{})
	go func() {
		defer close(closeFinished)
		c.persistActiveSessionBeforeClose()
	}()
	select {
	case <-closeFinished:
		t.Fatal("close took its snapshot while the staging handoff still held the persist lock")
	case <-time.After(100 * time.Millisecond):
	}
	stageAppendHook = func() {}
	close(releaseStaging)
	wait(runEntered, "the worker")
	wait(closeFinished, "the close persistence")
	close(releaseRun)
	wait(chatDone, "the chat turn")
	rows, err := c.sessionDB.GetMessagesAsConversation(c.ctx, c.sessionID, state.ConversationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var contents []any
	for _, row := range rows {
		contents = append(contents, row["content"])
	}
	if !reflect.DeepEqual(contents, []any{"old prompt", "old answer", "new prompt"}) {
		t.Fatalf("durable transcript %#v", contents)
	}
}

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
	patchTurn(t, func(_ context.Context, _ *agent.AIAgent, _ any, o agent.RunConversationOptions) (map[string]any, error) {
		_, envSet := os.LookupEnv(provider.TurnAuthorEnvName)
		calls = append(calls, map[string]any{"turn_author": o.TurnAuthor, "env_author_set": envSet})
		return map[string]any{"final_response": "done", "completed": true}, nil
	})
	initTurnAgent = func(c *CLI, _ string, _ *cliRuntime, _ map[string]any) bool {
		if c.agent == nil {
			c.agent = offlineAgent(t, c)
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
