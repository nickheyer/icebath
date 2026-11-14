package agent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Registered compute worker execution on session owned, the hermes_state conftest worker fixture
func workerFixture(t *testing.T) (*state.SessionDB, *RuntimeSessionStore) {
	t.Helper()
	caseHome(t)
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for id, prompt := range map[string]string{"owned": "prefix", "foreign": "secret"} {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "cli", SystemPrompt: prompt}); err != nil {
			t.Fatal(err)
		}
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RegisterWorkerExecution(ctx, state.WorkerRegistration{Epoch: epoch, ExecutionID: "worker", SessionID: "owned", Kind: "compute", AdoptionSecret: "secret"}); err != nil {
		t.Fatal(err)
	}
	scope := pyjson.ObjectOf("epoch", epoch, "execution_id", "worker", "session_id", "owned", "generation", int64(0))
	store := mustStore(t, dbWorkerCall(db), scope, filepath.Join(t.TempDir(), "outbox"), 0)
	t.Cleanup(store.closeOwner)
	return db, store
}

func newestUserRow(t *testing.T, db *state.SessionDB, sessionID string) [2]any {
	t.Helper()
	var content, apiContent sql.NullString
	if err := db.ExecuteWrite(t.Context(), func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(t.Context(), "SELECT content, api_content FROM messages WHERE session_id=? AND role='user' ORDER BY id DESC LIMIT 1", sessionID).Scan(&content, &apiContent)
	}); err != nil {
		t.Fatal(err)
	}
	out := [2]any{nil, nil}
	if content.Valid {
		out[0] = content.String
	}
	if apiContent.Valid {
		out[1] = apiContent.String
	}
	return out
}

func TestTurnPrologueRowBackfillsLandThroughTheWorkerLikeDirect(t *testing.T) {
	db, store := workerFixture(t)
	ctx := t.Context()
	if err := db.CreateSession(state.NewSession{ID: "direct", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	type durable struct {
		sidecar [2]any
		parts   any
	}
	got := map[string]durable{}
	for _, tc := range []struct {
		sid   string
		store SessionStore
	}{{"direct", db}, {"owned", store}} {
		a := &AIAgent{sessionDB: tc.store, sessionID: tc.sid, sessionPersistLock: &pythread.RLock{}, lastCompactionInPlace: true}
		text := []map[string]any{{"role": "user", "content": "question"}}
		if _, err := tc.store.ArchiveAndCompact(ctx, tc.sid, text, state.ArchiveCompactOptions{TailCount: 0}); err != nil {
			t.Fatal(err)
		}
		stampAPIContentSidecar(ctx, a, rowsToMessages(text), 0, "", "PLUGIN CONTEXT", true)
		sidecar := newestUserRow(t, db, tc.sid)
		if _, err := tc.store.AppendMessagesBatch(ctx, tc.sid, []map[string]any{{"role": "assistant", "content": "ok"}}, state.AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
		parts := []map[string]any{{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}}}
		if _, err := tc.store.AppendMessagesBatch(ctx, tc.sid, parts, state.AppendGuards{}, nil); err != nil {
			t.Fatal(err)
		}
		appendMultimodalContext(ctx, a, state.MessageFromDict(parts[0]), "", "PLUGIN CONTEXT", false)
		got[tc.sid] = durable{sidecar, newestUserRow(t, db, tc.sid)[0]}
	}
	want := durable{[2]any{"question", "question\n\nPLUGIN CONTEXT"}, "look\n[screenshot]\nPLUGIN CONTEXT"}
	if got["owned"] != want || got["direct"] != want {
		t.Fatalf("owned %v direct %v", got["owned"], got["direct"])
	}
	foreign, err := db.AppendMessage(ctx, "foreign", map[string]any{"role": "user", "content": "private"}, state.AppendGuards{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := store.SetUserMessageContent(ctx, "owned", foreign, "overwritten", nil); err != nil || n != 0 {
		t.Fatalf("foreign content rewrite %d %v", n, err)
	}
	if n, err := store.SetMessageAPIContent(ctx, "owned", foreign, "private", "injected"); err != nil || n != 0 {
		t.Fatalf("foreign api content rewrite %d %v", n, err)
	}
	if row := newestUserRow(t, db, "foreign"); row != [2]any{"private", nil} {
		t.Fatalf("foreign row %v", row)
	}
}
