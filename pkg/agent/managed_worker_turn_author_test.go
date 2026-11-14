package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Run recorder standing in for the worker's agent, the SimpleNamespace of the hermes case
type recordedWorkerTurn struct {
	text any
	o    RunConversationOptions
}

type workerTurnRecorder struct {
	seen  []recordedWorkerTurn
	notes string
	voice bool
}

func (r *workerTurnRecorder) RunConversationContent(_ context.Context, text any, o RunConversationOptions) (map[string]any, error) {
	r.seen = append(r.seen, recordedWorkerTurn{text, o})
	return map[string]any{"final_response": "ok"}, nil
}
func (r *workerTurnRecorder) SetVoiceTurnPending(pending bool)        { r.voice = pending }
func (r *workerTurnRecorder) SetGatewayTurnContextNotes(notes string) { r.notes = notes }
func (r *workerTurnRecorder) ValidToolNames() map[string]bool         { return nil }
func (r *workerTurnRecorder) Env() *tools.Env                         { return nil }
func (r *workerTurnRecorder) SessionID() string                       { return "" }
func (r *workerTurnRecorder) SessionStore() SessionStore              { return nil }

func TestWorkerTurnPassesTheAuthorToRunConversation(t *testing.T) {
	ctx := context.Background()
	agent := &workerTurnRecorder{}
	author := pyjson.ObjectOf("id", "alice", "name", "alice", "is_bot", true)
	frames := []*pyjson.Object{
		pyjson.ObjectOf("policy", pyjson.ObjectOf("kanban_json", nil), "text", "hello", "turn_author", author),
		pyjson.ObjectOf("policy", pyjson.ObjectOf("kanban_json", nil), "text", "again", "turn_author", nil),
	}
	for _, frame := range frames {
		if _, err := RunWorkerTurns(ctx, agent, frame, []map[string]any{}); err != nil {
			t.Fatal(err)
		}
	}
	want := []recordedWorkerTurn{
		{"hello", RunConversationOptions{ConversationHistory: []map[string]any{}, TurnAuthor: map[string]any{"id": "alice", "name": "alice", "is_bot": true}}},
		{"again", RunConversationOptions{ConversationHistory: []map[string]any{}}},
	}
	if !reflect.DeepEqual(agent.seen, want) {
		t.Fatalf("run_conversation calls %#v, want %#v", agent.seen, want)
	}
}
