package agent

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type retireFake struct{ calls []any }

func (f *retireFake) MemoryFlushPending(timeout time.Duration) bool {
	f.calls = append(f.calls, []any{"flush", int(timeout.Seconds())})
	return true
}

func (f *retireFake) RetiringSessionMessages() []map[string]any {
	return []map[string]any{{"role": "user", "content": "hi"}}
}

func (f *retireFake) ShutdownMemoryProvider(_ context.Context, messages []map[string]any) {
	f.calls = append(f.calls, []any{"shutdown", len(messages)})
}

func (f *retireFake) ReleaseClients(context.Context) { f.calls = append(f.calls, []any{"release"}) }

func TestRetireAgentFlushesPendingMemoryBeforeShutdown(t *testing.T) {
	agent := &retireFake{}
	RetireAgent(context.Background(), agent)
	want := []any{[]any{"flush", 10}, []any{"shutdown", 1}, []any{"release"}}
	if !reflect.DeepEqual(agent.calls, want) {
		t.Fatalf("calls %v", agent.calls)
	}
}
