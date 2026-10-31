package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Starts a workflow that runs until the app closes, so its id names a flow
func idleFlow(t *testing.T, m *Model, name string) *harness.Flow {
	t.Helper()
	w := harness.Workflow{Name: name, Run: func(ctx context.Context, f *harness.Flow, arg string) error {
		<-ctx.Done()
		return nil
	}}
	f, err := m.app.Flows.Start(m.ctx, w, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Opens a fresh session into the flow the way an OpenedMsg does
func openInto(t *testing.T, m *Model, flow string) *desk {
	t.Helper()
	live, err := m.app.Create()
	if err != nil {
		t.Fatal(err)
	}
	m.openFlowDesk(flow, live)
	d := m.deskOfLive(live)
	if d == nil {
		t.Fatal("no desk for the opened session")
	}
	return d
}

func TestFlowSessionsOverflowAndPrune(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	project := t.TempDir()
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	oldLayout(t, project, stored(t, st, project))
	m := started(t, project, "")
	m.wm.SetMotion(0)
	m.wm.SetSize(100, 30)
	f := idleFlow(t, m, "flowtest")
	before := len(m.wm.Workspaces())
	first := openInto(t, m, f.ID)
	primary := m.wm.WorkspaceByID(f.ID)
	if primary == nil || m.workspaceOf(first) != primary || m.wm.Active() != primary {
		t.Fatalf("first session of the flow: workspace %v active %v", m.workspaceOf(first), m.wm.Active())
	}
	var desks []*desk
	for i := 0; i < 6; i++ {
		desks = append(desks, openInto(t, m, f.ID))
	}
	spaces := m.flowWorkspaces(f.ID)
	if len(spaces) < 2 || spaces[0] != primary || len(m.wm.Workspaces()) != before+len(spaces) {
		t.Fatalf("flow workspaces %d of %d", len(spaces), len(m.wm.Workspaces()))
	}
	for i, ws := range spaces[1:] {
		if flow, overflow := m.flowOf(ws.ID); flow != f.ID || !overflow || overflowNumber(f.ID, ws.ID) != i+2 {
			t.Fatalf("overflow workspace %s", ws.ID)
		}
	}
	for _, d := range append(desks, first) {
		ws := m.workspaceOf(d)
		if flow, _ := m.flowOf(ws.ID); flow != f.ID {
			t.Fatalf("session seated outside the flow in %s", ws.ID)
		}
		if c := m.wm.Fits(ws, d.session); !c {
			t.Fatalf("session does not fit where it was seated, %s", ws.ID)
		}
	}
	if m.wm.Active() != primary {
		t.Fatal("overflow switched the stage away from the first workspace")
	}
	if flow, overflow := m.flowOf("w1"); flow != "" || overflow {
		t.Fatal("a plain workspace read as a flow")
	}
	if flow, overflow := m.flowOf(f.ID + ".x"); flow != "" || overflow {
		t.Fatal("a malformed overflow id read as a flow")
	}
	last := spaces[len(spaces)-1]
	for _, d := range m.desksIn(last) {
		m.closeSession(d)
	}
	if !last.Empty() {
		t.Fatal("overflow workspace not empty after closing its sessions")
	}
	m.pruneOverflow()
	if m.wm.WorkspaceByID(last.ID) != nil {
		t.Fatal("empty overflow workspace off the stage was kept")
	}
	shown := spaces[1]
	m.wm.Switch(m.wm.IndexOf(shown))
	for _, d := range m.desksIn(shown) {
		m.closeSession(d)
	}
	m.pruneOverflow()
	if m.wm.WorkspaceByID(shown.ID) == nil {
		t.Fatal("empty overflow workspace on the stage was pruned under the person")
	}
	m.wm.Switch(m.wm.IndexOf(primary))
	m.pruneOverflow()
	if m.wm.WorkspaceByID(shown.ID) != nil {
		t.Fatal("empty overflow workspace kept after the person left it")
	}
	for _, d := range m.desksIn(primary) {
		m.closeSession(d)
	}
	m.wm.Switch(0)
	m.pruneOverflow()
	if m.wm.WorkspaceByID(f.ID) == nil {
		t.Fatal("the workspace named after the flow was pruned")
	}
	again := openInto(t, m, f.ID)
	if m.workspaceOf(again) != primary {
		t.Fatal("a later session did not return to the emptied flow workspace")
	}
	next := m.overflowID(f.ID)
	if next != f.ID+".2" {
		t.Fatalf("next overflow id %s", next)
	}
}

func TestLayoutLeavesFlowWorkspacesOut(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, cfg)
	project := t.TempDir()
	st := &session.Store{Dir: filepath.Join(cfg, harness.SessionsDir)}
	oldLayout(t, project, stored(t, st, project))
	m := started(t, project, "")
	m.wm.SetMotion(0)
	m.wm.SetSize(80, 24)
	plain, _ := m.wm.AddWorkspace(m.workspaceID())
	f := idleFlow(t, m, "flowtest")
	for i := 0; i < 3; i++ {
		openInto(t, m, f.ID)
	}
	if len(m.flowWorkspaces(f.ID)) < 2 {
		t.Fatal("an 80 by 24 stage did not overflow three sessions")
	}
	m.wm.Switch(m.wm.IndexOf(m.flowWorkspaces(f.ID)[1]))
	l := m.layoutState()
	if len(l.Workspaces) != 2 {
		t.Fatalf("saved %d workspaces, want the two plain ones", len(l.Workspaces))
	}
	for _, wl := range l.Workspaces {
		if strings.HasPrefix(wl.ID, f.ID) {
			t.Fatalf("saved a flow workspace %s", wl.ID)
		}
	}
	if l.Active != 1 {
		t.Fatalf("active %d, want the last plain workspace before the flow", l.Active)
	}
	m.wm.Switch(m.wm.IndexOf(plain))
	if l := m.layoutState(); l.Active != 1 {
		t.Fatalf("active %d with the plain workspace on the stage", l.Active)
	}
	m.wm.Switch(0)
	if l := m.layoutState(); l.Active != 0 {
		t.Fatalf("active %d with the first workspace on the stage", l.Active)
	}
	_ = wm.Master
}
