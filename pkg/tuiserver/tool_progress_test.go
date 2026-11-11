package tuiserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Live session record under sid for one case, dropped at cleanup
func liveSession(t *testing.T, sid string, fields map[string]any) *tuigateway.Session {
	t.Helper()
	session := tuigateway.NewSession(fields)
	tuigateway.SetSessionRecord(sid, session)
	t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	return session
}

func TestTodoCompletionAlwaysEmitsSnapshotAndCompatEvent(t *testing.T) {
	log := childMirrorFixture(t)
	sid := "todo-state-test"
	session := liveSession(t, sid, map[string]any{"agent": nil, "edit_snapshots": map[string]any{}, "tool_started_at": map[string]float64{}, "tool_progress_mode": "off"})
	state := `{"todos": [{"id": "1", "content": "Work", "status": "in_progress"}], "revision": 9}`
	onToolComplete(context.Background(), sid, "call-1", "todo", map[string]any{}, state)
	got := log.all()
	if !reflect.DeepEqual(eventNames(got), []string{"tool.complete", "todo.updated"}) {
		t.Fatalf("events = %v", eventNames(got))
	}
	want := map[string]any{"todos": []any{map[string]any{"id": "1", "content": "Work", "status": "in_progress"}}, "revision": int64(9)}
	last := got[len(got)-1]
	if last.sid != sid || !reflect.DeepEqual(pyjson.Plain(objectOf(last.payload)), want) {
		t.Fatalf("todo.updated = %v", last)
	}
	cached, _ := session.Get("todo_state").(*pyjson.Object)
	if cached == nil || !reflect.DeepEqual(pyjson.Plain(cached), want) {
		t.Fatalf("todo_state = %v", session.Get("todo_state"))
	}
}

func TestNonTodoCompletionStaysSuppressedWhenProgressIsOff(t *testing.T) {
	log := childMirrorFixture(t)
	sid := "ordinary-tool-test"
	liveSession(t, sid, map[string]any{"agent": nil, "edit_snapshots": map[string]any{}, "tool_started_at": map[string]float64{}, "tool_progress_mode": "off"})
	onToolComplete(context.Background(), sid, "call-1", "terminal", map[string]any{}, "ok")
	if got := log.all(); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}

// Real agent whose live todo store sits at the given revision with no items
func todoAgent(t *testing.T, revision int) *agent.AIAgent {
	t.Helper()
	a := testAgent(t, agent.AIAgentOptions{Model: "test-model"})
	a.TodoStore().Restore([]any{}, revision)
	return a
}

func TestLiveSnapshotPrefersTheHighestRevision(t *testing.T) {
	cached, _ := pyjson.AsObject(map[string]any{"todos": []any{map[string]any{"id": "1", "content": "Current", "status": "pending"}}, "revision": 5})
	session := tuigateway.NewSession(map[string]any{"agent": todoAgent(t, 4), "todo_state": cached})
	payload := attachTodoState(map[string]any{}, session)
	state, _ := payload["todo_state"].(*pyjson.Object)
	if state == nil || todoRevision(state) != 5 {
		t.Fatalf("payload = %v", payload)
	}
}

func TestUnusedStoreIsNotAttached(t *testing.T) {
	session := tuigateway.NewSession(map[string]any{"agent": todoAgent(t, 0)})
	if payload := attachTodoState(map[string]any{}, session); payload["todo_state"] != nil {
		t.Fatalf("payload = %v", payload)
	}
}

func TestEmptyListAtNonzeroRevisionIsARealClear(t *testing.T) {
	raw, _ := pyjson.AsObject(map[string]any{"todos": []any{}, "revision": 2})
	state := tuigateway.NormalizeTodoState(raw)
	if state == nil || !reflect.DeepEqual(pyjson.Plain(state), map[string]any{"todos": []any{}, "revision": int64(2)}) {
		t.Fatalf("state = %v", state)
	}
}

func TestSubagentLifecycleBypassesToolProgressOff(t *testing.T) {
	log := childMirrorFixture(t)
	sid := "subagent-progress-off"
	liveSession(t, sid, map[string]any{"agent": nil, "tool_progress_mode": "off"})
	onToolProgress(context.Background(), sid, "subagent.start", "delegate_task", "goal", map[string]any{"goal": "goal", "subagent_id": "s1"})
	onToolProgress(context.Background(), sid, "tool.output_risk", "terminal", nil,
		map[string]any{"tool_call_id": "t1", "risk_metadata": map[string]any{"risk": "high", "findings": []any{}}})
	if got := eventNames(log.all()); !reflect.DeepEqual(got, []string{"subagent.start"}) {
		t.Fatalf("events = %v", got)
	}
}

func TestMoaReferenceRelayedWithLabelAndIndex(t *testing.T) {
	log := childMirrorFixture(t)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	onToolProgress(context.Background(), "sid-1", "moa.reference", "openrouter:openai/gpt-5.5", "Paris is the capital of France.",
		map[string]any{"moa_index": 1, "moa_count": 2})
	got := log.all()
	if len(got) != 1 || got[0].event != "moa.reference" || got[0].sid != "sid-1" {
		t.Fatalf("events = %v", got)
	}
	payload := got[0].payload
	if payload["label"] != "openrouter:openai/gpt-5.5" || payload["text"] != "Paris is the capital of France." ||
		payload["index"] != 1 || payload["count"] != 2 {
		t.Fatalf("payload = %v", payload)
	}
}
