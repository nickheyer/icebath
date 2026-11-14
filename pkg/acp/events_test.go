package acp

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyacp"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type recConn struct {
	mu      sync.Mutex
	updates []pyacp.SessionUpdate
	calls   int
}

func (c *recConn) SessionUpdate(ctx context.Context, sid string, u pyacp.SessionUpdate, meta *pyjson.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.updates = append(c.updates, u)
	return nil
}

type syncLoop struct{ refuse error }

func (l syncLoop) RunCoroutineThreadsafe(fn func(ctx context.Context) (any, error)) (Future, error) {
	if l.refuse != nil {
		return nil, l.refuse
	}
	r, err := fn(context.Background())
	return &fakeFuture{result: r, err: err}, nil
}

func progressStatuses(c *recConn) []string {
	var out []string
	for _, u := range c.updates {
		if p, ok := u.(*pyacp.ToolCallProgress); ok {
			out = append(out, *p.Status)
		}
	}
	return out
}

func TestToolProgressCallbackDuplicateSameNameToolCallsUseFifoIds(t *testing.T) {
	conn, calls := &recConn{}, NewToolCalls()
	progress := MakeToolProgressCb(conn, "session-1", syncLoop{}, calls, nil, nil)
	step := MakeStepCb(conn, "session-1", syncLoop{}, calls, nil)
	progress("tool.started", "terminal", "$ ls", map[string]any{"command": "ls"}, nil)
	progress("tool.started", "terminal", "$ pwd", map[string]any{"command": "pwd"}, nil)
	if len(calls.IDs["terminal"]) != 2 {
		t.Fatal(calls.IDs)
	}
	step(1, []map[string]any{{"name": "terminal", "result": "ok-1"}})
	if len(calls.IDs["terminal"]) != 1 {
		t.Fatal(calls.IDs)
	}
	step(2, []map[string]any{{"name": "terminal", "result": "ok-2"}})
	if _, ok := calls.IDs["terminal"]; ok {
		t.Fatal(calls.IDs)
	}
}

func TestStepCallbackFalseyResultReachesClientUnchanged(t *testing.T) {
	for _, c := range []struct {
		raw      any
		expected string
	}{{"", ""}, {int64(0), "0"}, {false, "False"}} {
		conn, calls := &recConn{}, NewToolCalls()
		calls.IDs["terminal"] = []string{"tc-f"}
		MakeStepCb(conn, "session-1", syncLoop{}, calls, nil)(1, []map[string]any{{"name": "terminal", "result": c.raw}})
		p := conn.updates[0].(*pyacp.ToolCallProgress)
		if p.ToolCallID != "tc-f" || contentText(t, p.Content[0]) != c.expected {
			t.Fatalf("raw %v update %#v", c.raw, p)
		}
	}
}

func TestStepCallbackToolProgressCapturesSnapshotMetadata(t *testing.T) {
	prevID, prevSnap := newToolCallID, captureLocalEditSnapshot
	snap := &tools.LocalEditSnapshot{}
	newToolCallID = func() string { return "tc-meta" }
	captureLocalEditSnapshot = func(*tools.Env, string, map[string]any, *string) *tools.LocalEditSnapshot { return snap }
	t.Cleanup(func() { newToolCallID, captureLocalEditSnapshot = prevID, prevSnap })
	conn, calls := &recConn{}, NewToolCalls()
	MakeToolProgressCb(conn, "session-1", syncLoop{}, calls, nil, nil)("tool.started", "write_file", "", map[string]any{"path": "diff-test.txt", "content": "hello"}, nil)
	if !reflect.DeepEqual(calls.IDs["write_file"], []string{"tc-meta"}) {
		t.Fatal(calls.IDs)
	}
	m := calls.Meta["tc-meta"]
	if !pyjson.Equal(pyjson.Plain(m.Args), map[string]any{"path": "diff-test.txt", "content": "hello"}) || m.Snapshot != snap {
		t.Fatalf("meta %#v", m)
	}
	if conn.calls != 1 {
		t.Fatal(conn.calls)
	}
}

func TestStepCallbackTodoCompletionEmitsNativePlanUpdateAfterToolCompletion(t *testing.T) {
	conn, calls := &recConn{}, NewToolCalls()
	calls.IDs["todo"] = []string{"tc-todo"}
	todo := `{"todos":[{"id":"inspect","content":"Inspect ACP","status":"completed"},{"id":"patch","content":"Patch renderer","status":"in_progress"},{"id":"old","content":"Drop stale task","status":"cancelled"}],"summary":{"total":3}}`
	MakeStepCb(conn, "session-1", syncLoop{}, calls, nil)(1, []map[string]any{{"name": "todo", "result": todo}})
	if len(conn.updates) != 2 {
		t.Fatal(len(conn.updates))
	}
	if _, ok := conn.updates[0].(*pyacp.ToolCallProgress); !ok {
		t.Fatalf("first %T", conn.updates[0])
	}
	plan := conn.updates[1].(*pyacp.AgentPlanUpdate)
	var contents, statuses, prios []string
	for _, e := range plan.Entries {
		contents, statuses, prios = append(contents, e.Content), append(statuses, e.Status), append(prios, e.Priority)
	}
	if !reflect.DeepEqual(contents, []string{"Inspect ACP", "Patch renderer", "[cancelled] Drop stale task"}) ||
		!reflect.DeepEqual(statuses, []string{"completed", "in_progress", "completed"}) ||
		!reflect.DeepEqual(prios, []string{"medium", "medium", "medium"}) {
		t.Fatalf("plan %#v", plan)
	}
}

func TestSendUpdateSchedulerFailureClosesUpdateCoroutine(t *testing.T) {
	conn := &recConn{}
	sendUpdate(conn, "session-1", syncLoop{refuse: errors.New("scheduler down")}, pyacp.UpdateAgentMessageText("noop"))
	if conn.calls != 0 {
		t.Fatal("update ran")
	}
}

func TestAssistantMessageIdsDeltasShareOneUuidUntilFlush(t *testing.T) {
	conn, ids := &recConn{}, &AssistantMessageIDAllocator{}
	cb := MakeMessageCb(conn, "s", syncLoop{}, ids)
	cb("Hello ")
	cb("")
	cb("world")
	cb(nil)
	cb("next turn")
	m := func(i int) string { return *conn.updates[i].(*pyacp.AgentMessageChunk).MessageID }
	if m(0) != m(1) || m(2) == m(0) {
		t.Fatal("ids")
	}
	if _, err := uuid.Parse(m(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(m(2)); err != nil {
		t.Fatal(err)
	}
}

func TestAssistantMessageIdsThoughtChunksCarryId(t *testing.T) {
	conn, ids := &recConn{}, &AssistantMessageIDAllocator{}
	MakeThinkingCb(conn, "s", syncLoop{}, ids)("pondering")
	MakeMessageCb(conn, "s", syncLoop{}, ids)("answer")
	if *conn.updates[0].(*pyacp.AgentThoughtChunk).MessageID != *conn.updates[1].(*pyacp.AgentMessageChunk).MessageID {
		t.Fatal("ids differ")
	}
}

func TestAssistantMessageIdsNoAllocatorKeepsLegacyShape(t *testing.T) {
	conn := &recConn{}
	MakeMessageCb(conn, "s", syncLoop{}, nil)("text")
	if conn.updates[0].(*pyacp.AgentMessageChunk).MessageID != nil {
		t.Fatal("id set")
	}
}

func TestToolCallsAlwaysReachATerminalStatusToolCompletedClosesTheCallOnceAndTheStepCloserStandsDown(t *testing.T) {
	conn, calls, turn := &recConn{}, NewToolCalls(), &TurnState{}
	calls.IDs["read"] = []string{"tc-1", "tc-2"}
	calls.Meta["tc-1"] = ToolCallMeta{Args: obj("path", "a")}
	progress := MakeToolProgressCb(conn, "s", syncLoop{}, calls, nil, turn)
	step := MakeStepCb(conn, "s", syncLoop{}, calls, turn)
	progress("tool.completed", "read", "", nil, map[string]any{"result": "file body"})
	step(2, []map[string]any{{"name": "read", "result": "file body", "arguments": `{"path": "a"}`}})
	if len(conn.updates) != 1 || conn.updates[0].(*pyacp.ToolCallProgress).ToolCallID != "tc-1" {
		t.Fatalf("updates %#v", conn.updates)
	}
	if _, ok := calls.Meta["tc-1"]; ok || !reflect.DeepEqual(calls.IDs["read"], []string{"tc-2"}) {
		t.Fatal(calls.IDs)
	}
}

func TestToolCallsAlwaysReachATerminalStatusStepFallbackCoercesWireArgumentsAndTurnEndFlushFailsWhatIsStillOpen(t *testing.T) {
	conn, calls := &recConn{}, NewToolCalls()
	calls.IDs["write_file"] = []string{"tc-1"}
	calls.IDs["edit"] = []string{"tc-denied"}
	calls.Meta["tc-1"] = ToolCallMeta{Args: obj("path", "a")}
	calls.Meta["tc-denied"] = ToolCallMeta{Args: obj()}
	MakeStepCb(conn, "s", syncLoop{}, calls, &TurnState{})(2, []map[string]any{{"name": "write_file", "result": "ok", "arguments": `{"path": "a", "content": "x"}`}})
	if !reflect.DeepEqual(progressStatuses(conn), []string{"completed"}) {
		t.Fatal(progressStatuses(conn))
	}
	if FlushOpenToolCalls(conn, "s", syncLoop{}, calls) != 1 || FlushOpenToolCalls(conn, "s", syncLoop{}, calls) != 0 {
		t.Fatal("flush counts")
	}
	if !reflect.DeepEqual(progressStatuses(conn), []string{"completed", "failed"}) || len(calls.IDs) != 0 || len(calls.Meta) != 0 {
		t.Fatal(progressStatuses(conn))
	}
}

func TestToolCallsAlwaysReachATerminalStatusToolCompletedIsErrorFlagClosesTheCallAsFailed(t *testing.T) {
	conn, calls := &recConn{}, NewToolCalls()
	calls.IDs["terminal"] = []string{"tc-1"}
	MakeToolProgressCb(conn, "s", syncLoop{}, calls, nil, nil)("tool.completed", "terminal", "", nil,
		map[string]any{"is_error": true, "result": "[Tool execution cancelled — terminal was skipped. User sent a new message]"})
	if !reflect.DeepEqual(progressStatuses(conn), []string{"failed"}) {
		t.Fatal(progressStatuses(conn))
	}
}
