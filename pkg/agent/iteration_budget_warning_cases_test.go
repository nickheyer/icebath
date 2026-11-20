package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestCheckpointRearmsPerTurnWithoutChangingBudgetOrDurableRows(t *testing.T) {
	contents := map[string]func() any{
		"string": func() any { return "result" },
		"parts":  func() any { return []any{map[string]any{"type": "text", "text": "result"}} },
	}
	for name, content := range contents {
		for _, interrupted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s interrupted=%v", name, interrupted), func(t *testing.T) {
				env := budgetCheckpointEnv(t)
				a := budgetCheckpointAgent(t, context.Background(), env, "0.75")
				ctx := tools.WithToolEnv(context.Background(), env)
				for turn := range 2 {
					resetPerTurnAgentState(ctx, a)
					a.interruptRequested.Store(interrupted)
					for range 3 {
						a.iterationBudget.Load().Consume()
					}
					id := fmt.Sprintf("t%d", turn)
					messages := rowsToMessages([]map[string]any{{"role": "user", "content": fmt.Sprintf("work %d", turn)},
						{"role": "assistant", "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}}}},
						{"role": "tool", "tool_call_id": id, "content": content()}})
					host := a.toolExecutor().Host
					host.BeforeToolFlush(&messages)
					if persisted := host.FlushMessagesToSessionDB(messages); persisted != state.FlushPersisted {
						t.Fatalf("checkpoint flush %v", persisted)
					}
					stored, err := a.sessionDB.GetMessagesAsConversation(ctx, a.sessionID, state.ConversationOptions{})
					if err != nil {
						t.Fatal(err)
					}
					notices := 0
					for _, row := range stored {
						if strings.Contains(fmt.Sprint(row["content"]), "iteration budget checkpoint") {
							notices++
						}
					}
					want := turn + 1
					if interrupted {
						want = 0
					}
					if notices != want {
						t.Fatalf("turn %d stored notices %d want %d", turn, notices, want)
					}
					last := fmt.Sprint(messagesToRows(messages)[2]["content"])
					if strings.Contains(last, "3 of 4") == interrupted {
						t.Fatalf("turn %d tool result %q", turn, last)
					}
					snapshot := messagesToRows(messages)
					a.interruptRequested.Store(false)
					s := &LoopState{Messages: messages, APICallCount: 3}
					if err := prepareIteration(ctx, a, s); err != nil {
						t.Fatal(err)
					}
					if got := messagesToRows(s.Messages); !reflect.DeepEqual(got, snapshot) {
						t.Fatalf("turn %d durable rows rewritten %v", turn, got)
					}
					if !a.iterationBudget.Load().Consume() || a.iterationBudget.Load().Consume() {
						t.Fatalf("turn %d budget changed", turn)
					}
				}
			})
		}
	}
}
