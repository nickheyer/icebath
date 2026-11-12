package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Store with one session of source holding a hello and hi exchange
func kanbanGuardDB(t *testing.T, source, sid string) *state.SessionDB {
	t.Helper()
	tuiTestHome(t)
	t.Setenv("ICEBATH_KANBAN_TASK", "")
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: sid, Source: source}); err != nil {
		t.Fatal(err)
	}
	oneshotAppend(t, db, sid, "user", "hello")
	oneshotAppend(t, db, sid, "assistant", "hi")
	return db
}

func TestOneshotResumeRefusesKanbanSession(t *testing.T) {
	db := kanbanGuardDB(t, "kanban", "worker-1")
	_, _, _, err := loadOneshotResumeTarget(context.Background(), db, "worker-1")
	if err == nil || !strings.Contains(err.Error(), "cannot resume session worker-1") {
		t.Fatalf("got %v", err)
	}
}

func TestOneshotResumeOrdinarySessionStillWorks(t *testing.T) {
	db := kanbanGuardDB(t, "cli", "chat-1")
	sid, history, _, err := loadOneshotResumeTarget(context.Background(), db, "chat-1")
	if err != nil || sid != "chat-1" || len(history) != 2 {
		t.Fatalf("got %q %v %v", sid, history, err)
	}
}
