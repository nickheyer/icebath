package app

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

// One spawn the review chokepoint handed on
type refineSpawn struct {
	messagesSnapshot []map[string]any
	explicit         bool
}

// Agent wearing the real review chokepoint with the immediate spawn recorded so no fork runs
type refineChokepointAgent struct {
	*GatewayAgent
	spawned  []refineSpawn
	enqueued int
}

func (a *refineChokepointAgent) SpawnBackgroundReview(ctx context.Context, messagesSnapshot []map[string]any, reviewMemory, reviewSkills bool, focus *string, explicit bool) {
	agent.SpawnBackgroundReviewOn(ctx, a, messagesSnapshot, reviewMemory, reviewSkills, focus, explicit)
}

func (a *refineChokepointAgent) ValidToolNames() map[string]bool {
	return map[string]bool{"memory": true}
}

func (a *refineChokepointAgent) BackgroundReviewDelegateDepth() int { return 0 }

func (a *refineChokepointAgent) BackgroundReviewShouldDefer(context.Context, map[string]any) bool {
	return false
}

func (a *refineChokepointAgent) EnqueueBackgroundReview(context.Context, []map[string]any, bool, bool, *string, map[string]any, bool) {
	a.enqueued++
}

func (a *refineChokepointAgent) SpawnBackgroundReviewNow(_ context.Context, messagesSnapshot []map[string]any, _, _ bool, _ *string, _ map[string]any, explicit bool) {
	a.spawned = append(a.spawned, refineSpawn{messagesSnapshot: messagesSnapshot, explicit: explicit})
}

var _ agent.BackgroundReviewSelf = (*refineChokepointAgent)(nil)

// History whose user content and assistant tool calls are nested containers
func refineNestedHistory() []map[string]any {
	return []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "ask"}}},
		{"role": "assistant", "content": "ok", "tool_calls": []any{map[string]any{
			"id": "call-1", "function": map[string]any{"name": "read_file", "arguments": `{"path":"x"}`},
		}}},
	}
}

// Whether two containers are the same object
func sameContainer(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	return va.Kind() == vb.Kind() && va.UnsafePointer() == vb.UnsafePointer()
}

// Snapshot equals the live history, shares none of its containers, and mutating it leaves the history intact
func assertRefineIsolated(t *testing.T, live, snapshot []map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(snapshot, live) {
		t.Fatalf("snapshot %v, want %v", snapshot, live)
	}
	for i := range live {
		if sameContainer(snapshot[i], live[i]) {
			t.Fatalf("message %d is shared with the live history", i)
		}
		for _, key := range []string{"content", "tool_calls"} {
			switch live[i][key].(type) {
			case map[string]any, []any:
				if sameContainer(snapshot[i][key], live[i][key]) {
					t.Fatalf("message %d %s is shared with the live history", i, key)
				}
			}
		}
	}
	snapshot[0]["content"].([]any)[0].(map[string]any)["text"] = "mutated"
	snapshot[1]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] = "{}"
	if got := live[0]["content"].([]any)[0].(map[string]any)["text"]; got != "ask" {
		t.Fatalf("live user text %v", got)
	}
	if got := live[1]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"]; got != `{"path":"x"}` {
		t.Fatalf("live tool arguments %v", got)
	}
}

// CLI holding the chokepoint agent over the nested history
func refineCLI(t *testing.T) (*CLI, *refineChokepointAgent) {
	t.Helper()
	c := chatCLI(t)
	double := &refineChokepointAgent{GatewayAgent: offlineAgent(t, c)}
	c.setAgent(double)
	c.conversationHistory = refineNestedHistory()
	return c, double
}

func TestCliRefineSnapshotDoesNotAliasLiveHistory(t *testing.T) {
	c, double := refineCLI(t)
	c.handleRefineCommand("/refine")
	if len(double.spawned) != 1 || double.enqueued != 0 {
		t.Fatalf("spawned %d enqueued %d", len(double.spawned), double.enqueued)
	}
	assertRefineIsolated(t, c.conversationHistory, double.spawned[0].messagesSnapshot)
}

func TestCliRefineReachesSpawnAsExplicit(t *testing.T) {
	c, double := refineCLI(t)
	c.handleRefineCommand("/refine")
	if len(double.spawned) != 1 {
		t.Fatalf("spawned %d", len(double.spawned))
	}
	if !double.spawned[0].explicit {
		t.Fatal("the /refine spawn is not explicit")
	}
}
