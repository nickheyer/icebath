package tuiserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Launch store holding a kanban worker transcript and an ordinary chat
func kanbanResumeDB(t *testing.T) {
	t.Helper()
	h := sessionCaseScope(t)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	useSharedDB(t, db)
	for _, row := range []state.NewSession{{ID: "worker-1", Source: "kanban"}, {ID: "chat-1", Source: "tui"}} {
		if err := db.CreateSession(row); err != nil {
			t.Fatal(err)
		}
	}
}

// Resumes a stored session through the request handler with its messages omitted
func kanbanResume(sessionID string) map[string]any {
	return handleRequest(context.Background(), map[string]any{"id": "r-" + sessionID, "method": "session.resume",
		"params": map[string]any{"session_id": sessionID, "omit_messages": true}})
}

// Whether any live session holds the stored key
func liveForKey(key string) bool {
	for _, live := range tuigateway.LiveSessions() {
		if live.Session.Get("session_key") == key {
			return true
		}
	}
	return false
}

func TestGatewayResumeRefusesKanbanWorkerSession(t *testing.T) {
	kanbanResumeDB(t)
	resp := kanbanResume("worker-1")
	failure, _ := resp["error"].(map[string]any)
	if failure["code"] != 4132 {
		t.Fatalf("response %v", resp)
	}
	if message, _ := failure["message"].(string); !strings.Contains(message, "Kanban") {
		t.Fatalf("message %v", failure["message"])
	}
	if liveForKey("worker-1") {
		t.Fatal("the refused transcript was built into a live session")
	}
}

func TestGatewayResumeOrdinarySessionUnchanged(t *testing.T) {
	kanbanResumeDB(t)
	resp := kanbanResume("chat-1")
	if _, failed := resp["error"]; failed {
		t.Fatalf("response %v", resp)
	}
	if !liveForKey("chat-1") {
		t.Fatal("the ordinary session never resumed")
	}
}
