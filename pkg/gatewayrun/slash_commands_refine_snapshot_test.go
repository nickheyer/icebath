package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/fakellm"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Transcript whose user content and assistant tool calls are nested containers the fork sanitizes on its wire
func refineNestedHistory() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "ask"}}},
		{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{
			"id": "call-1", "function": map[string]any{"name": "read_file", "arguments": `{"path":"x"}`}}}},
	}
}

// Message of a request body with the role, nil when none has it
func requestMessage(body map[string]any, role string) map[string]any {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && m["role"] == role {
			return m
		}
	}
	return nil
}

// Waits until the review fork logged the release of its clients, the last step of its worker
func awaitForkClientsReleased(homeDir string) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := os.ReadFile(filepath.Join(homeDir, "logs", "agent.log"))
		if strings.Contains(string(raw), "OpenAI client closed (cache_evict") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// test_refine_snapshot_isolation.py test_gateway_refine_snapshot_does_not_alias_live_history
func TestGatewayRefineSnapshotDoesNotAliasLiveHistory(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Cleanup(func() { awaitForkClientsReleased(homeDir) })
	ctx := context.Background()
	bodies := make(chan map[string]any, 8)
	server, err := fakellm.NewResponder(func(req *fakellm.Request) any {
		bodies <- req.Body
		return fakellm.Text{Text: "Nothing to save."}
	}, fakellm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Stop)
	a, err := agent.NewAIAgent(ctx, agent.AIAgentOptions{
		Model: "anthropic/claude-sonnet-4", APIKey: "test", BaseURL: server.BaseURL(), Provider: "openrouter",
		MaxIterations: 5, QuietMode: true, SkipContextFiles: true, SkipMemory: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	live := refineNestedHistory()
	a.SetUnflushedSessionMessagesForTests(live)
	r := allocGatewayRunner()
	r.agentCache = pydict.New[string, agentCacheEntry]()
	r.agentCacheLock = &sync.Mutex{}
	source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "1", ChatType: "dm"}
	key, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	r.agentCache.Set(key, agentCacheEntry{agent: a})
	if _, err := r.handleRefineCommand(ctx, &gateway.MessageEvent{Text: "/refine", Source: source, AllowGatewayControl: true}); err != nil {
		t.Fatal(err)
	}
	run := a.CurrentBackgroundReviewRun()
	if run == nil {
		t.Fatal("/refine did not reach the review chokepoint")
	}
	var body map[string]any
	select {
	case body = <-bodies:
	case <-time.After(20 * time.Second):
		t.Fatal("the review fork never reached the model")
	}
	user, assistant := requestMessage(body, "user"), requestMessage(body, "assistant")
	if user == nil || assistant == nil || user["content"] != "ask" || assistant["content"] != "ok" {
		t.Fatalf("the snapshot did not reach the fork: %v", body["messages"].([]any)[1:])
	}
	finished := make(chan struct{})
	go func() {
		run.WaitRequestDone()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("the review fork never finished")
	}
	if !reflect.DeepEqual(live, refineNestedHistory()) {
		t.Fatalf("the fork rewrote the live history %v", live)
	}
	got := agent.SessionMessageDicts(a.SessionMessages())
	gotParts, _ := got[0]["content"].([]any)
	gotCalls, _ := got[1]["tool_calls"].([]any)
	if len(gotParts) == 0 || len(gotCalls) == 0 ||
		gotParts[0].(map[string]any)["text"] != "ask" ||
		gotCalls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"path":"x"}` {
		t.Fatalf("the fork rewrote the agent's session messages %v", got)
	}
}
