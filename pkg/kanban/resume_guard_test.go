package kanban

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

type guardDB struct {
	source       string
	flipSourceTo string
	parent       string
	endReason    string
	sessionID    string
}

// Session store holding one session of the given source with a two message transcript
func newGuardDB(t *testing.T, o guardDB) *state.SessionDB {
	t.Helper()
	clearKanbanEnv(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.CreateSession(state.NewSession{ID: o.sessionID, Source: o.source, ParentSessionID: o.parent}); err != nil {
		t.Fatal(err)
	}
	if o.flipSourceTo != "" {
		if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
			_, werr := conn.ExecContext(ctx, "UPDATE sessions SET source = ? WHERE id = ?", o.flipSourceTo, o.sessionID)
			return werr
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendGuardMessage(t, db, o.sessionID, "user", "hello")
	appendGuardMessage(t, db, o.sessionID, "assistant", "hi")
	if o.endReason != "" {
		if err := db.EndSession(o.sessionID, o.endReason); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func appendGuardMessage(t *testing.T, db *state.SessionDB, sessionID, role, content string) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), sessionID, map[string]any{"role": role, "content": content}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func mustOwned(t *testing.T, db *state.SessionDB, sessionID string) bool {
	t.Helper()
	owned, err := db.IsKanbanOwnedSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return owned
}

func TestKanbanSourceRowIsKanbanOwned(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", sessionID: "worker-1"})
	if !mustOwned(t, db, "worker-1") {
		t.Fatal("kanban source row not owned")
	}
}

func TestCliSourceRowIsNotKanbanOwned(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "cli", sessionID: "chat-1"})
	if mustOwned(t, db, "chat-1") {
		t.Fatal("cli source row owned")
	}
}

func TestCreatedSourceSurvivesSurfaceFlip(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", flipSourceTo: "cli", sessionID: "flipped-1"})
	if !mustOwned(t, db, "flipped-1") {
		t.Fatal("flipped worker row lost its ownership")
	}
}

func TestCompressionLineageOfKanbanWorkerIsOwned(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", sessionID: "worker-root", endReason: "compression"})
	if err := db.CreateSession(state.NewSession{ID: "worker-tip", Source: "cli", ParentSessionID: "worker-root"}); err != nil {
		t.Fatal(err)
	}
	appendGuardMessage(t, db, "worker-tip", "user", "continued")
	if !mustOwned(t, db, "worker-tip") {
		t.Fatal("compression child of a worker not owned")
	}
}

func TestBranchChildOfKanbanWorkerIsNotOwned(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", sessionID: "worker-root", endReason: "blocked"})
	if err := db.CreateSession(state.NewSession{ID: "branch-child", Source: "cli", ParentSessionID: "worker-root",
		ModelConfig: map[string]any{"_branched_from": "worker-root"}}); err != nil {
		t.Fatal(err)
	}
	if mustOwned(t, db, "branch-child") {
		t.Fatal("branch child swept into the guard")
	}
}

func TestGuardRefusesKanbanSessionWithReason(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", sessionID: "worker-1"})
	refusal := KanbanResumeRefusal(context.Background(), db, "worker-1")
	if refusal == "" || !strings.Contains(refusal, "worker-1") || !strings.Contains(refusal, "Kanban") {
		t.Fatalf("refusal %q", refusal)
	}
}

func TestGuardAllowsOrdinarySession(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "cli", sessionID: "chat-1"})
	if refusal := KanbanResumeRefusal(context.Background(), db, "chat-1"); refusal != "" {
		t.Fatalf("ordinary session refused %q", refusal)
	}
}

func TestGuardExemptsDispatcherOwnedRun(t *testing.T) {
	db := newGuardDB(t, guardDB{source: "kanban", sessionID: "worker-1"})
	t.Setenv("ICEBATH_KANBAN_TASK", "task-42")
	if refusal := KanbanResumeRefusal(context.Background(), db, "worker-1"); refusal != "" {
		t.Fatalf("dispatcher owned run refused %q", refusal)
	}
}

type failingOwnershipProbe struct{}

func (failingOwnershipProbe) IsKanbanOwnedSession(context.Context, string) (bool, error) {
	return false, errors.New("database is locked")
}

func TestGuardFailsOpenOnProbeError(t *testing.T) {
	clearKanbanEnv(t)
	if refusal := KanbanResumeRefusal(context.Background(), failingOwnershipProbe{}, "chat-1"); refusal != "" {
		t.Fatalf("probe error refused %q", refusal)
	}
}

func TestGuardLeavesAStoreWithoutTheProbeUnclassified(t *testing.T) {
	clearKanbanEnv(t)
	var missing *state.SessionDB
	for _, db := range []any{nil, missing, struct{}{}} {
		if refusal := KanbanResumeRefusal(context.Background(), db, "chat-1"); refusal != "" {
			t.Fatalf("store %T refused %q", db, refusal)
		}
	}
}
