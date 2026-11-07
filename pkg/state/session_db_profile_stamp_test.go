package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func stampRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "workprof"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	return root
}

// Owner column of one session read through a fresh raw connection after the store closed
func stampedProfile(t *testing.T, path, sessionID string) any {
	t.Helper()
	conn := openRaw(t, path)
	defer conn.Close()
	rows, err := connRows(context.Background(), conn, "SELECT profile_name FROM sessions WHERE id = ?", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		return nil
	}
	return rows[0]["profile_name"]
}

func stampCreate(t *testing.T, path string, n NewSession) {
	t.Helper()
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(n); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultStoreStampsDefault(t *testing.T) {
	path := filepath.Join(stampRoot(t), "state.db")
	stampCreate(t, path, NewSession{ID: "s_default", Source: "cli"})
	if got := stampedProfile(t, path, "s_default"); got != "default" {
		t.Fatal(got)
	}
}

func TestNamedProfileStoreStampsOwnName(t *testing.T) {
	path := filepath.Join(stampRoot(t), "profiles", "workprof", "state.db")
	stampCreate(t, path, NewSession{ID: "s_prof", Source: "desktop"})
	if got := stampedProfile(t, path, "s_prof"); got != "workprof" {
		t.Fatal(got)
	}
}

func TestExplicitProfileNameWins(t *testing.T) {
	path := filepath.Join(stampRoot(t), "state.db")
	stampCreate(t, path, NewSession{ID: "s_explicit", Source: "cli", ProfileName: "llm-wiki"})
	if got := stampedProfile(t, path, "s_explicit"); got != "llm-wiki" {
		t.Fatal(got)
	}
}

func TestStoreOutsideProfileTreeNeverGuesses(t *testing.T) {
	stampRoot(t)
	path := filepath.Join(t.TempDir(), "elsewhere", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stampCreate(t, path, NewSession{ID: "s_outside", Source: "cli"})
	if got := stampedProfile(t, path, "s_outside"); got != nil {
		t.Fatal(got)
	}
}

func TestCompressionChildOfNullParentIsStamped(t *testing.T) {
	path := filepath.Join(stampRoot(t), "state.db")
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "s_parent", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET profile_name = NULL WHERE id = 's_parent'")
	if err := db.PublishCompressionChild(context.Background(), CompressionChildPublish{ParentSessionID: "s_parent", ChildSessionID: "s_child",
		Source: "cli", Messages: []map[string]any{{"role": "user", "content": "hi"}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stampedProfile(t, path, "s_child"); got != "default" {
		t.Fatal(got)
	}
}

func TestPeerSelfHealInsertIsStamped(t *testing.T) {
	path := filepath.Join(stampRoot(t), "state.db")
	db := mustSessionDB(t, path, false)
	user, key, chat := "u1", "k1", "c1"
	if err := db.RecordGatewaySessionPeer(context.Background(), "s_selfheal", GatewayPeer{Source: "telegram", UserID: &user, SessionKey: &key, ChatID: &chat}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stampedProfile(t, path, "s_selfheal"); got != "default" {
		t.Fatal(got)
	}
}

func TestLegacyBackfillStillTargetsOnlyNull(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(stampRoot(t), "state.db")
	db := mustSessionDB(t, path, false)
	if err := db.CreateSession(NewSession{ID: "s_new", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(NewSession{ID: "s_owned", Source: "cli", ProfileName: "llm-wiki"}); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET profile_name = NULL WHERE id = 's_new'")
	for _, want := range []int{1, 0} {
		if n, err := db.BackfillNullSessionProfiles(ctx, "workprof"); n != want || err != nil {
			t.Fatal(n, want, err)
		}
	}
	if n, err := db.BackfillNullSessionProfiles(ctx, "  "); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := stampedProfile(t, path, "s_new"); got != "workprof" {
		t.Fatal(got)
	}
	if got := stampedProfile(t, path, "s_owned"); got != "llm-wiki" {
		t.Fatal(got)
	}
}

func TestBackfillPromotesCwdFromModelConfig(t *testing.T) {
	ctx := context.Background()
	db := usageDB(t)
	for _, n := range []NewSession{{ID: "legacy", Source: "acp", Model: "m", ModelConfig: map[string]any{"cwd": "/work/hs-wwd"}},
		{ID: "cli", Source: "cli", Model: "m", ModelConfig: map[string]any{"cwd": "/somewhere"}},
		{ID: "bare", Source: "acp", Model: "m", ModelConfig: map[string]any{"provider": "anthropic"}}} {
		if err := db.CreateSession(n); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := db.BackfillACPSessionCwd(ctx); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	for id, want := range map[string]string{"legacy": "/work/hs-wwd", "cli": "", "bare": ""} {
		if got := crossField(t, db, id, "cwd"); pyStrOr(got) != want {
			t.Fatalf("%s = %v", id, got)
		}
	}
	if n, err := db.BackfillACPSessionCwd(ctx); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

func TestBackfillNeverOverwritesAnExistingCwd(t *testing.T) {
	db := usageDB(t)
	if err := db.CreateSession(NewSession{ID: "fine", Source: "acp", Model: "m", Cwd: "/real/path", ModelConfig: map[string]any{"cwd": "/stale/json/path"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := db.BackfillACPSessionCwd(context.Background()); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if got := crossField(t, db, "fine", "cwd"); got != "/real/path" {
		t.Fatal(got)
	}
}
