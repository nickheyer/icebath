package acp

import (
	"context"
	"reflect"
	"testing"

	"github.com/google/uuid"
	acputil "github.com/nickheyer/icebath/pkg/utils/acp"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const todosJSON = `[{"id": "1", "content": "write the test", "status": "completed"}, {"id": "2", "content": "fix the bug", "status": "in_progress"}, {"id": "3", "content": "drop the hack", "status": "cancelled"}]`

func mappingAgent(t *testing.T, reply string) (*GatewayACPAgent, *fakeGateway, *editorFake) {
	t.Helper()
	a := newGatewayAgent(t)
	link := newFakeGateway(func(ctx context.Context, method string, params map[string]any) (*jsonutil.Object, error) {
		return jsonObj(t, reply), nil
	})
	editor := &editorFake{}
	a.loop.enter()
	a.snapshots.Set("s", jsonutil.NewObject())
	a.gateway, a.conn = link, editor
	a.loop.leave()
	return a, link, editor
}

func projectEvent(t *testing.T, a *GatewayACPAgent, kind, payload, aid string) {
	t.Helper()
	event := jsonutil.ObjectOf("session_id", "s", "type", kind, "admission_id", aid, "payload", jsonObj(t, payload))
	if err := onLoop(a, func() error { return a.project(context.Background(), event) }); err != nil {
		t.Fatal(err)
	}
}

func updatesOf(e *editorFake) []acputil.SessionUpdate {
	var out []acputil.SessionUpdate
	for _, u := range e.seen() {
		out = append(out, u.update)
	}
	return out
}

type closedCall struct{ id, status string }

func closedCalls(e *editorFake) []closedCall {
	var out []closedCall
	for _, u := range updatesOf(e) {
		if p, ok := u.(*acputil.ToolCallProgress); ok {
			status := ""
			if p.Status != nil {
				status = *p.Status
			}
			out = append(out, closedCall{p.ToolCallID, status})
		}
	}
	return out
}

func TestTodoUpdatedBecomesTheNativePlanUpdate(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "todo.updated", `{"todos": `+todosJSON+`, "revision": 2}`, "a")
	sent := updatesOf(editor)
	if len(sent) != 1 {
		t.Fatalf("updates = %v", sent)
	}
	plan, ok := sent[0].(*acputil.AgentPlanUpdate)
	if !ok {
		t.Fatalf("update is %T", sent[0])
	}
	var got [][2]string
	for _, entry := range plan.Entries {
		got = append(got, [2]string{entry.Content, entry.Status})
	}
	want := [][2]string{{"write the test", "completed"}, {"fix the bug", "in_progress"}, {"[cancelled] drop the hack", "completed"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %v", got)
	}
}

func TestToolCompleteHonoursTheExecutorsIsErrorVerdict(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "tool.start", `{"tool_call_id": "c1", "tool_name": "terminal", "args": {"command": "make"}}`, "a")
	projectEvent(t, a, "tool.complete", `{"tool_call_id": "c1", "tool_name": "terminal", "args": {"command": "make"}, "is_error": true, "result": "cancelled by the user"}`, "a")
	closed := closedCalls(editor)
	if len(closed) == 0 || closed[len(closed)-1].status != "failed" {
		t.Fatalf("closed = %v", closed)
	}
}

func TestAToolCallLeftOpenIsClosedWhenTheTurnCompletes(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "tool.start", `{"tool_call_id": "blocked", "tool_name": "terminal", "args": {"command": "rm"}}`, "a")
	projectEvent(t, a, "tool.start", `{"tool_call_id": "other", "tool_name": "terminal", "args": {}}`, "b")
	projectEvent(t, a, "message.complete", `{"text": "done", "outcome": "completed"}`, "a")
	if closed := closedCalls(editor); !reflect.DeepEqual(closed, []closedCall{{"blocked", "failed"}}) {
		t.Fatalf("closed = %v", closed)
	}
	if !onLoop(a, func() bool { return a.toolArgs.Has(toolKey{"s", "other"}) }) {
		t.Fatal("another admission's call was closed")
	}
}

func TestAnAnsweredPermissionCardIsClosed(t *testing.T) {
	for _, tc := range []struct{ choice, status string }{{"allow_once", "completed"}, {"deny", "failed"}, {"", "failed"}} {
		t.Run("choice_"+tc.choice, func(t *testing.T) {
			a, _, editor := mappingAgent(t, `{"status": "resolved"}`)
			var card *acputil.ToolCallUpdate
			editor.permission = func(options []acputil.PermissionOption, sessionID string, toolCall *acputil.ToolCallUpdate) (*acputil.RequestPermissionResponse, error) {
				card = toolCall
				if tc.choice == "" {
					return &acputil.RequestPermissionResponse{Outcome: &acputil.DeniedOutcome{}}, nil
				}
				return &acputil.RequestPermissionResponse{Outcome: &acputil.AllowedOutcome{OptionID: tc.choice}}, nil
			}
			prompt := jsonObj(t, `{"kind": "approval", "prompt_id": "p", "execution_generation": 1, "command": "rm -r x", "description": "recursive delete", "choices": ["once", "deny"]}`)
			onLoop(a, func() bool { return a.answerPermission(context.Background(), "s", prompt) })
			closed := closedCalls(editor)
			if card == nil || len(closed) != 1 || closed[0] != (closedCall{card.ToolCallID, tc.status}) {
				t.Fatalf("closed = %v card %v", closed, card)
			}
		})
	}
}

func TestAnAnsweredClarifyCardIsClosed(t *testing.T) {
	a, _, editor := mappingAgent(t, `{"status": "resolved"}`)
	editor.permission = func([]acputil.PermissionOption, string, *acputil.ToolCallUpdate) (*acputil.RequestPermissionResponse, error) {
		return &acputil.RequestPermissionResponse{Outcome: &acputil.AllowedOutcome{OptionID: "choice-0"}}, nil
	}
	prompt := jsonObj(t, `{"kind": "clarify", "prompt_id": "q", "execution_generation": 1, "question": "Which?", "choices": ["A", "B"]}`)
	onLoop(a, func() bool { return a.answerClarify(context.Background(), "s", prompt) })
	if closed := closedCalls(editor); !reflect.DeepEqual(closed, []closedCall{{"clarify-q", "completed"}}) {
		t.Fatalf("closed = %v", closed)
	}
}

func chunkIDs(t *testing.T, e *editorFake) ([]string, []string) {
	t.Helper()
	var ids, texts []string
	for _, u := range updatesOf(e) {
		var id *string
		var content acputil.ContentBlock
		switch update := u.(type) {
		case *acputil.AgentMessageChunk:
			id, content = update.MessageID, update.Content
		case *acputil.AgentThoughtChunk:
			id, content = update.MessageID, update.Content
		default:
			continue
		}
		if id == nil || *id == "" {
			t.Fatalf("chunk without a message id: %#v", u)
		}
		ids = append(ids, *id)
		texts = append(texts, content.(*acputil.TextContentBlock).Text)
	}
	return ids, texts
}

func TestAssistantChunksCarryAMessageIdThatChangesAtEachNewMessage(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "reasoning.delta", `{"text": "think"}`, "a")
	projectEvent(t, a, "message.delta", `{"text": "Let me look. "}`, "a")
	projectEvent(t, a, "tool.start", `{"tool_call_id": "c1", "tool_name": "read_file", "args": {"path": "/x"}}`, "a")
	projectEvent(t, a, "tool.complete", `{"tool_call_id": "c1", "tool_name": "read_file", "args": {"path": "/x"}, "is_error": false, "result": "{\"content\": \"x\"}"}`, "a")
	projectEvent(t, a, "message.delta", `{"text": "It says "}`, "a")
	projectEvent(t, a, "message.complete", `{"text": "It says x.", "outcome": "completed"}`, "a")
	projectEvent(t, a, "message.delta", `{"text": "next turn"}`, "b")
	ids, _ := chunkIDs(t, editor)
	if len(ids) != 5 {
		t.Fatalf("ids = %v", ids)
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("message id %q: %v", id, err)
		}
	}
	if ids[0] != ids[1] || ids[1] == ids[2] || ids[2] != ids[3] || ids[3] == ids[4] {
		t.Fatalf("ids = %v", ids)
	}
}

func TestAReconnectNeverReusesTheOpenReplyId(t *testing.T) {
	a, _, editor := mappingAgent(t, `{}`)
	projectEvent(t, a, "message.delta", `{"text": "half a reply"}`, "a")
	onLoop(a, func() bool { a.teardown(); return true })
	onLoop(a, func() bool { a.conn = editor; return true })
	projectEvent(t, a, "message.delta", `{"text": "the next reply"}`, "b")
	ids, _ := chunkIDs(t, editor)
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("ids = %v", ids)
	}
}

func TestATerminalReceiptFinishesTheReplyLikeMessageComplete(t *testing.T) {
	a, link, editor := mappingAgent(t, `{"result": {"final_response": "Done."}}`)
	projectEvent(t, a, "message.delta", `{"text": "Look"}`, "a")
	projectEvent(t, a, "tool.start", `{"tool_call_id": "c1", "tool_name": "terminal", "args": {}}`, "a")
	receipt := jsonObj(t, `{"admission_id": "a", "outcome": "completed"}`)
	if err := onLoop(a, func() error { return a.settledReceipt(context.Background(), link, "s", "s", receipt, nil) }); err != nil {
		t.Fatal(err)
	}
	projectEvent(t, a, "message.delta", `{"text": "next"}`, "b")
	var ids, texts []string
	for _, u := range updatesOf(editor) {
		if chunk, ok := u.(*acputil.AgentMessageChunk); ok {
			texts = append(texts, chunk.Content.(*acputil.TextContentBlock).Text)
			id := ""
			if chunk.MessageID != nil {
				id = *chunk.MessageID
			}
			ids = append(ids, id)
		}
	}
	if !reflect.DeepEqual(texts, []string{"Look", "Done.", "next"}) {
		t.Fatalf("texts = %v", texts)
	}
	if ids[1] == "" || ids[1] == ids[2] {
		t.Fatalf("ids = %v", ids)
	}
	if closed := closedCalls(editor); !reflect.DeepEqual(closed, []closedCall{{"c1", "failed"}}) {
		t.Fatalf("closed = %v", closed)
	}
}
