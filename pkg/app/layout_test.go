package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

func stored(t *testing.T, st *session.Store, project string) string {
	t.Helper()
	s := session.New()
	s.Label(session.ProjectLabel, project)
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	if err := st.Save(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

func TestClaimProjectLocksOneInstance(t *testing.T) {
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	dir, first, notice := claimProject("/p/a")
	if first == nil || notice != "" {
		t.Fatalf("first claim: %v %q", first, notice)
	}
	defer first.Release()
	again, second, notice := claimProject("/p/a")
	if again != dir || second != nil || notice == "" {
		t.Fatalf("second claim on the same project should fail with a notice: %s %v %q", again, second, notice)
	}
	other, third, notice := claimProject("/p/b")
	if third == nil || notice != "" || other == dir {
		t.Fatalf("claim on another project: %s %v %q", other, third, notice)
	}
	third.Release()
}

func TestLoadLayoutMigratesOnlyOwnSessions(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	mine := stored(t, st, "/p/a")
	theirs := stored(t, st, "/p/b")
	orphan := stored(t, st, "")
	legacy := layoutState{Layout: wm.Layout{Theme: "light", Workspaces: []wm.WorkspaceLayout{
		{ID: "w1", Tiles: []wm.TileLayout{{Key: "session:" + theirs}, {Key: "metrics"}}, Focus: "session:" + theirs},
		{ID: "w2", Tiles: []wm.TileLayout{{Key: "session:" + mine}, {Key: "session:" + theirs}}, Floats: []wm.FloatLayout{{Key: "session:" + orphan}}},
	}, Active: 1}, Missions: map[string]string{mine: "m1", theirs: "m2"}}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(cfg, layoutFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir, err := harness.StatePath("/p/a")
	if err != nil {
		t.Fatal(err)
	}
	l, raw, ok := loadLayout(stateDir, "/p/a", st)
	if !ok || raw != nil {
		t.Fatalf("legacy layout not taken up: %v %v", ok, raw)
	}
	if l.Theme != "light" {
		t.Fatalf("preferences lost: %+v", l.Layout)
	}
	if len(l.Workspaces) != 1 {
		t.Fatalf("workspaces: %+v", l.Workspaces)
	}
	keys := sessionKeys(l.Workspaces[0])
	if len(keys) != 2 || keys[0] != mine || keys[1] != orphan {
		t.Fatalf("sessions of the kept workspace: %v", keys)
	}
	if _, ok := l.Missions[theirs]; ok || l.Missions[mine] != "m1" {
		t.Fatalf("missions: %v", l.Missions)
	}
	if err := os.WriteFile(filepath.Join(stateDir, layoutFile), []byte(`{"theme":"contrast"}`), 0o644); err != nil {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stateDir, layoutFile), []byte(`{"theme":"contrast"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l, raw, ok = loadLayout(stateDir, "/p/a", st)
	if !ok || raw == nil || l.Theme != "contrast" {
		t.Fatalf("own layout not preferred: %v %+v", ok, l.Layout)
	}
}
