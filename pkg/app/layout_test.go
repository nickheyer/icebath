package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Starts the app on the project the way the shell does, shut down when the test ends
func started(t *testing.T, project, resume string) *Model {
	t.Helper()
	cfg, err := harness.Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), Options{Config: cfg, Dir: project, Resume: resume})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.shutdown() })
	return m
}

// Writes a layout into the state directory of the project, with one stored session id and one mark on an unknown profile
func oldLayout(t *testing.T, project, id string) {
	t.Helper()
	dir, err := harness.StatePath(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	l := layoutState{Layout: wm.Layout{Workspaces: []wm.WorkspaceLayout{
		{ID: "w1", Tiles: []wm.TileLayout{{Key: "session:" + id}, {Key: "session:@2"}, {Key: "mission:" + id + ":m1"}, {Key: "metrics"}}},
	}}, Sessions: map[string]templateSession{"@2": {Profile: "missing", Model: "gone"}}}
	data, _ := json.Marshal(l)
	if err := os.WriteFile(filepath.Join(dir, layoutFile), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func deskIDs(m *Model) []string {
	var out []string
	for _, d := range m.desks {
		out = append(out, d.live.Session.ID())
	}
	return out
}

// Fails when the saved form of the layout names any session id or lays out other than two marks and the metrics panel
func checkSaved(t *testing.T, m *Model, ids ...string) {
	t.Helper()
	l := m.layoutState()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range append(ids, deskIDs(m)...) {
		if strings.Contains(string(data), id) {
			t.Fatalf("saved layout names session %s: %s", id, data)
		}
	}
	if len(l.Workspaces) != 1 || len(l.Sessions) != 2 {
		t.Fatalf("saved layout: %s", data)
	}
	var keys []string
	for _, tile := range l.Workspaces[0].Tiles {
		keys = append(keys, tile.Key)
	}
	if strings.Join(keys, " ") != "session:@1 session:@2 metrics" {
		t.Fatalf("saved tiles: %v", keys)
	}
}

func TestLayoutOpensFreshSessions(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	project := t.TempDir()
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	old := stored(t, st, project)
	oldLayout(t, project, old)
	m := started(t, project, "")
	if m.instance == nil {
		t.Fatal("first instance should hold the project")
	}
	ids := deskIDs(m)
	if len(ids) != 2 || len(m.wm.Workspaces()) != 1 {
		t.Fatalf("desks %v in %d workspaces", ids, len(m.wm.Workspaces()))
	}
	for _, d := range m.desks {
		if d.live.Session.ID() == old || d.live.Session.Len() != 0 {
			t.Fatalf("desk on a stored session %s", d.live.Session.ID())
		}
	}
	if m.app.Store.Leased(old) {
		t.Fatal("stored session leased by a fresh start")
	}
	checkSaved(t, m, old)
}

func TestResumedSessionTakesFirstMark(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	project := t.TempDir()
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	old := stored(t, st, project)
	oldLayout(t, project, old)
	m := started(t, project, old)
	ids := deskIDs(m)
	if len(ids) != 2 || ids[0] != old || len(m.wm.Workspaces()) != 1 {
		t.Fatalf("desks %v in %d workspaces", ids, len(m.wm.Workspaces()))
	}
	if d := m.current(); d == nil || d.live.Session.ID() != old {
		t.Fatal("resumed session not on the stage")
	}
	checkSaved(t, m, old)
}

func TestSecondInstanceLaysOutLayout(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	project := t.TempDir()
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	oldLayout(t, project, stored(t, st, project))
	first := started(t, project, "")
	second := started(t, project, "")
	if second.instance != nil {
		t.Fatal("second instance took the project lock")
	}
	a, b := deskIDs(first), deskIDs(second)
	if len(b) != 2 || len(second.wm.Workspaces()) != 1 {
		t.Fatalf("second instance desks %v", b)
	}
	for _, id := range b {
		if slices.Contains(a, id) {
			t.Fatalf("both instances opened %s", id)
		}
	}
}
