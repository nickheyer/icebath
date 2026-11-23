package tuiserver

import (
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Real agent whose live todo store sits at the given revision with no items
func todoAgent(t *testing.T, revision int) *agent.AIAgent {
	t.Helper()
	a := testAgent(t, agent.AIAgentOptions{Model: "test-model"})
	a.TodoStore().Restore([]any{}, revision)
	return a
}

func TestEmptyListAtNonzeroRevisionIsARealClear(t *testing.T) {
	raw, _ := pyjson.AsObject(map[string]any{"todos": []any{}, "revision": 2})
	state := tuihost.NormalizeTodoState(raw)
	if state == nil || !reflect.DeepEqual(pyjson.Plain(state), map[string]any{"todos": []any{}, "revision": int64(2)}) {
		t.Fatalf("state = %v", state)
	}
}
