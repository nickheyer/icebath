package gatewayapi

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// One agent turn over the stored history, the user row stamped by append and the sequence repaired
func dedupeAgentTurn(t *testing.T, history []any, userMessage, answer string) map[string]any {
	t.Helper()
	messages := make([]map[string]any, 0, len(history)+2)
	for _, h := range history {
		messages = append(messages, asMap(h))
	}
	state.AppendMessage(&messages, map[string]any{"role": "user", "content": userMessage}, nil)
	if _, err := state.RepairMessageSequence(&messages); err != nil {
		t.Fatal(err)
	}
	state.AppendMessage(&messages, map[string]any{"role": "assistant", "content": answer, "reasoning": "r", "finish_reason": "stop"}, nil)
	return map[string]any{"messages": rowsAsAny(messages)}
}

func TestChainedResponsesTurnsStoreEachMessageOnce(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	history := []any{}
	var counts []int
	for turn, prompt := range []string{"Remember TOKEN-ABC.", "What token?", "Third"} {
		answer := fmt.Sprintf("answer %d", turn)
		history = buildResponseConversationHistory(history, prompt, dedupeAgentTurn(t, history, prompt, answer), answer, 0)
		counts = append(counts, len(history))
	}
	if !reflect.DeepEqual(counts, []int{2, 4, 6}) {
		t.Fatalf("counts %v", counts)
	}
	var roles []any
	for _, m := range history {
		roles = append(roles, rowField(m, "role"))
	}
	if !reflect.DeepEqual(roles, []any{"user", "assistant", "user", "assistant", "user", "assistant"}) {
		t.Fatalf("roles %v", roles)
	}
}
