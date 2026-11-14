package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func seedSearchDB(t *testing.T, dir, sessionID, content string) *state.SessionDB {
	t.Helper()
	db := openOwnerDB(t, filepath.Join(dir, "state.db"))
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessagesBatch(context.Background(), sessionID, []map[string]any{{"role": "user", "content": content},
		{"role": "assistant", "content": "noted"}}, state.AppendGuards{}, nil); err != nil {
		t.Fatal(err)
	}
	return db
}

func searchedSessions(t *testing.T, out string) (bool, []any) {
	t.Helper()
	payload, err := pyjson.LoadsDict(out)
	if err != nil {
		t.Fatal(out)
	}
	var ids []any
	results, _ := payload.GetOr("results", nil).([]any)
	for _, r := range results {
		ids = append(ids, r.(*pyjson.Object).GetOr("session_id", nil))
	}
	success, _ := payload.GetOr("success", nil).(bool)
	return success, ids
}

func TestWorkerRecallReadsTheStateDbInsteadOfTheStore(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	db := seedSearchDB(t, homeDir, "worker-s", "the zanzibar rollout is blocked on the store")
	defer db.Close()
	store := mustStore(t, cannedCall(func(string, *pyjson.Object) (any, error) { return pyjson.NewObject(), nil }),
		pyjson.ObjectOf("session_id", "worker-s"), filepath.Join(homeDir, "outbox"), 0)
	defer store.Close()
	if _, searchable := any(store).(tools.SessionSearchStore); searchable {
		t.Fatal("the worker store answers FTS reads")
	}
	a := &AIAgent{sessionDB: store, env: &tools.Env{Home: homeDir}}
	defer a.finalizeOwnedSessionRow()
	recall := a.SessionDBForRecall(ctx)
	if recall == nil {
		t.Fatal("worker recall has no database")
	}
	success, ids := searchedSessions(t, tools.SessionSearch(ctx, tools.SessionSearchArgs{Query: "zanzibar", DB: recall}))
	if !success || len(ids) != 1 || ids[0] != "worker-s" {
		t.Fatalf("results %v %v", success, ids)
	}
}

func TestAReadableHandleIsStillUsedAsGiven(t *testing.T) {
	ctx := context.Background()
	homeDir := t.TempDir()
	homeDB := seedSearchDB(t, homeDir, "home-session", "zanzibar appears only in the home transcript")
	defer homeDB.Close()
	elsewhere := t.TempDir()
	passed := seedSearchDB(t, elsewhere, "passed-in", "zanzibar from the caller's handle")
	defer passed.Close()
	a := &AIAgent{sessionDB: passed, env: &tools.Env{Home: homeDir}}
	_, ids := searchedSessions(t, tools.SessionSearch(ctx, tools.SessionSearchArgs{Query: "zanzibar", DB: a.SessionDBForRecall(ctx)}))
	if len(ids) != 1 || ids[0] != "passed-in" {
		t.Fatalf("results %v", ids)
	}
}
