package commands

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/state"
)

// Session store at path for one test step, closed by the caller
func openProfileStore(t *testing.T, path string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), path, false)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRenameProfileMigrateIdentityCommandRepairsAFailedLiveMigration(t *testing.T) {
	tmp := profileTestHome(t)
	ctx := context.Background()
	var out, errOut bytes.Buffer
	t.Cleanup(manage.SwapOutput(&out, &errOut))
	if _, err := manage.CreateProfile(ctx, "oldname", manage.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir := filepath.Join(tmp, ".icebath", "profiles", "oldname")
	pdb := openProfileStore(t, filepath.Join(oldDir, "state.db"))
	if err := pdb.CreateSession(state.NewSession{ID: "sess1", Source: "feishu", SessionKey: "agent:oldname:feishu:dm:chatA",
		ProfileName: "oldname", ChatID: "chatA", ChatType: "dm"}); err != nil {
		t.Fatal(err)
	}
	pdb.Close()
	scope := filepath.Join(tmp, ".icebath", "sessions")
	rootPath := filepath.Join(tmp, ".icebath", "state.db")
	root := openProfileStore(t, rootPath)
	if err := root.SaveGatewayRoutingEntry(ctx, "agent:oldname:feishu:dm:chatA",
		`{"session_key": "agent:oldname:feishu:dm:chatA", "session_id": "sess1", "origin": {"platform": "feishu", "chat_id": "chatA", "profile": "oldname"}}`, scope); err != nil {
		t.Fatal(err)
	}
	root.Close()
	t.Cleanup(manage.SwapCheckAliasCollision(func(string) (string, error) { return "skip", nil }))
	h := &profiles.GatewayStatusHooks
	live := true
	h.RecordedServedProfiles = func() ([]string, bool, error) { return []string{"default"}, live, nil }
	h.NotifyProfilesChanged = func(string) {}
	h.MigrateProfileIdentity = func(string, string, string) (any, error) { return nil, nil }
	h.Identify = func(string) (any, error) { return nil, nil }
	if _, err := manage.RenameProfile(ctx, "oldname", "newname"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "icebath profile migrate-identity oldname newname") {
		t.Fatalf("stderr %q", errOut.String())
	}
	live = false
	first, _, err := runProfileCmd(ProfileArgs{Action: "migrate-identity", OldName: "oldname", NewName: "newname"})
	if err != nil || !strings.Contains(first, "✓ Session/routing identity migrated") {
		t.Fatalf("%q %v", first, err)
	}
	if _, _, err := runProfileCmd(ProfileArgs{Action: "migrate-identity", OldName: "oldname", NewName: "newname"}); err != nil {
		t.Fatalf("second run %v", err)
	}
	moved := openProfileStore(t, filepath.Join(tmp, ".icebath", "profiles", "newname", "state.db"))
	var key, profile string
	found := false
	err = moved.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, "SELECT session_key, profile_name FROM sessions WHERE id = ?", "sess1")
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			found = true
			return rows.Scan(&key, &profile)
		}
		return rows.Err()
	})
	moved.Close()
	if err != nil || !found || key != "agent:newname:feishu:dm:chatA" || profile != "newname" {
		t.Fatalf("%v %v %q %q", err, found, key, profile)
	}
	root2 := openProfileStore(t, rootPath)
	defer root2.Close()
	routing, err := root2.LoadGatewayRoutingEntries(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := routing["agent:oldname:feishu:dm:chatA"]; stale {
		t.Fatalf("old key kept %v", routing)
	}
	if _, moved := routing["agent:newname:feishu:dm:chatA"]; !moved {
		t.Fatalf("new key missing %v", routing)
	}
}
