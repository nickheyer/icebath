package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

func fixtureManager(db *state.SessionDB) *SessionManager {
	return NewSessionManager(func() any { return &nsAgent{model: "fixture"} }, db)
}

func firstContent(t *testing.T, db *state.SessionDB, sid string) any {
	t.Helper()
	messages, err := db.GetMessagesAsConversation(context.Background(), sid, state.ConversationOptions{})
	if err != nil || len(messages) == 0 {
		t.Fatalf("messages of %s = %v, %v", sid, messages, err)
	}
	return messages[0]["content"]
}

func TestNewSessionPersistsOnlyWhenContentExists(t *testing.T) {
	db := provenanceDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	m := fixtureManager(db)
	st := mustCreate(t, m, dir)
	if getRow(t, db, st.SessionID) != nil {
		t.Fatal("an empty session was persisted")
	}
	m.UpdateCwd(ctx, st.SessionID, filepath.Join(dir, "moved"))
	m.SaveSession(ctx, st.SessionID)
	emptyFork, err := m.ForkSession(ctx, st.SessionID, ".")
	if err != nil {
		t.Fatal(err)
	}
	if getRow(t, db, st.SessionID) != nil || getRow(t, db, emptyFork.SessionID) != nil {
		t.Fatal("an empty session or fork was persisted")
	}
	st.History = append(st.History, map[string]any{"role": "user", "content": "kept content"})
	m.SaveSession(ctx, st.SessionID)
	fork, err := m.ForkSession(ctx, st.SessionID, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, sid := range []string{st.SessionID, fork.SessionID} {
		if got := getRow(t, db, sid).GetOr("source", nil); got != "acp" {
			t.Fatalf("source of %s = %v", sid, got)
		}
		if got := firstContent(t, db, sid); got != "kept content" {
			t.Fatalf("content of %s = %v", sid, got)
		}
	}
}

func TestExistingEmptyHistoryStillUpdatesMetadata(t *testing.T) {
	db := provenanceDB(t)
	dir := t.TempDir()
	ctx := context.Background()
	m := fixtureManager(db)
	if err := db.CreateSession(state.NewSession{ID: "existing", Source: "acp", Model: "original"}); err != nil {
		t.Fatal(err)
	}
	st := m.GetSession(ctx, "existing")
	if st == nil || len(st.History) != 0 {
		t.Fatalf("state = %+v", st)
	}
	st.Model = "selected-model"
	m.UpdateCwd(ctx, st.SessionID, filepath.Join(dir, "selected"))
	row := getRow(t, db, st.SessionID)
	if row.GetOr("model", nil) != "selected-model" {
		t.Fatalf("model = %v", row.GetOr("model", nil))
	}
	if got := jsonField(t, row, "model_config").GetOr("cwd", nil); got != st.Cwd {
		t.Fatalf("cwd = %v, want %s", got, st.Cwd)
	}
	if m.GetSession(ctx, st.SessionID) != st {
		t.Fatal("a second state was built")
	}
	zero := 0.0
	never, err := db.ListNeverActiveKeyedSessions(ctx, zero)
	if err != nil || len(never) != 0 {
		t.Fatalf("never active = %v, %v", never, err)
	}
}

func TestCreatedSessionRecordsCwdInItsOwnColumn(t *testing.T) {
	db := provenanceDB(t)
	workspace := filepath.Join(t.TempDir(), "hs-wwd")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	m := fixtureManager(db)
	st := mustCreate(t, m, workspace)
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	m.SaveSession(context.Background(), st.SessionID)
	row := getRow(t, db, st.SessionID)
	if row.GetOr("cwd", nil) != st.Cwd {
		t.Fatalf("cwd column = %v", row.GetOr("cwd", nil))
	}
	if got := jsonField(t, row, "model_config").GetOr("cwd", nil); got != st.Cwd {
		t.Fatalf("model_config cwd = %v", got)
	}
}

func emptyColumn(v any) bool { return v == nil || v == "" }

func TestCwdIsPromotedWhenTheRowAlreadyExists(t *testing.T) {
	db := provenanceDB(t)
	workspace := filepath.Join(t.TempDir(), "app-1")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	m := fixtureManager(db)
	st := mustCreate(t, m, workspace)
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	if err := db.CreateSession(state.NewSession{ID: st.SessionID, Source: "acp", Model: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if !emptyColumn(getRow(t, db, st.SessionID).GetOr("cwd", nil)) {
		t.Fatal("cwd column set before the save")
	}
	m.SaveSession(context.Background(), st.SessionID)
	if got := getRow(t, db, st.SessionID).GetOr("cwd", nil); got != st.Cwd {
		t.Fatalf("cwd column = %v", got)
	}
}

func TestReopeningInAnotherWorkspaceMovesTheCwdColumn(t *testing.T) {
	db := provenanceDB(t)
	dir := t.TempDir()
	first, second := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	for _, p := range []string{first, second} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	m := fixtureManager(db)
	st := mustCreate(t, m, first)
	st.History = append(st.History, map[string]any{"role": "user", "content": "hello"})
	m.SaveSession(ctx, st.SessionID)
	m.UpdateCwd(ctx, st.SessionID, second)
	row := getRow(t, db, st.SessionID)
	cwd, _ := row.GetOr("cwd", nil).(string)
	if cwd != st.Cwd || !strings.Contains(cwd, second) {
		t.Fatalf("cwd column = %q", cwd)
	}
	generation, _ := pyFloat(row.GetOr("git_metadata_generation", nil))
	if generation < 1 {
		t.Fatalf("generation = %v", row.GetOr("git_metadata_generation", nil))
	}
}

func TestLegacyRowsGetTheirCwdColumnBackfilledWhenTheManagerOpensTheDb(t *testing.T) {
	db := provenanceDB(t)
	for _, n := range []state.NewSession{
		{ID: "legacy", Source: "acp", Model: "m", ModelConfig: map[string]any{"cwd": "/work/hs-wwd"}},
		{ID: "cli", Source: "cli", Model: "m", ModelConfig: map[string]any{"cwd": "/somewhere"}},
	} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if !emptyColumn(getRow(t, db, "legacy").GetOr("cwd", nil)) {
		t.Fatal("legacy cwd column set before the manager opened the store")
	}
	m := fixtureManager(db)
	mustCreate(t, m, t.TempDir())
	if got := getRow(t, db, "legacy").GetOr("cwd", nil); got != "/work/hs-wwd" {
		t.Fatalf("legacy cwd = %v", got)
	}
	if !emptyColumn(getRow(t, db, "cli").GetOr("cwd", nil)) {
		t.Fatal("a cli row was repaired")
	}
}

func TestUpdatesModelConfig(t *testing.T) {
	db := provenanceDB(t)
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "acp", Model: "gpt-4"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionMeta(ctx, "s1", `{"cwd": "/new/path", "provider": "openai"}`, nil); err != nil {
		t.Fatal(err)
	}
	stored := jsonField(t, getRow(t, db, "s1"), "model_config")
	if stored.GetOr("cwd", nil) != "/new/path" || stored.GetOr("provider", nil) != "openai" {
		t.Fatalf("stored = %v", stored.Vals)
	}
}

func TestCwdPersistedViaUpdateSessionMeta(t *testing.T) {
	db := provenanceDB(t)
	ctx := context.Background()
	m := NewSessionManager(mockFactory, db)
	st := mustCreate(t, m, "/original")
	st.History = append(st.History, map[string]any{"role": "user", "content": "hi"})
	m.SaveSession(ctx, st.SessionID)
	if getRow(t, db, st.SessionID) == nil {
		t.Fatal("session row missing")
	}
	st.Cwd = "/updated"
	m.SaveSession(ctx, st.SessionID)
	if got := jsonField(t, getRow(t, db, st.SessionID), "model_config").GetOr("cwd", nil); got != "/updated" {
		t.Fatalf("cwd = %v", got)
	}
}

func TestModelPersistedViaUpdateSessionMeta(t *testing.T) {
	db := provenanceDB(t)
	ctx := context.Background()
	m := NewSessionManager(mockFactory, db)
	st := mustCreate(t, m, ".")
	st.History = append(st.History, map[string]any{"role": "user", "content": "hi"})
	m.SaveSession(ctx, st.SessionID)
	st.Model = "new-model-xyz"
	m.SaveSession(ctx, st.SessionID)
	if got := getRow(t, db, st.SessionID).GetOr("model", nil); got != "new-model-xyz" {
		t.Fatalf("model = %v", got)
	}
}

func TestExistingModelNotClearedOnSave(t *testing.T) {
	db := provenanceDB(t)
	ctx := context.Background()
	m := NewSessionManager(mockFactory, db)
	st := mustCreate(t, m, ".")
	st.History = append(st.History, map[string]any{"role": "user", "content": "hi"})
	m.SaveSession(ctx, st.SessionID)
	stored := "stored-model"
	if err := db.UpdateSessionMeta(ctx, st.SessionID, `{"cwd": "."}`, &stored); err != nil {
		t.Fatal(err)
	}
	st.Model = ""
	m.SaveSession(ctx, st.SessionID)
	if got := getRow(t, db, st.SessionID).GetOr("model", nil); got != "stored-model" {
		t.Fatalf("model = %v", got)
	}
}
