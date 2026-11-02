package sprint

import (
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Environment with a theme and keymap and nothing of the harness, as the arena sees it outside the app
func testEnv() *cards.Env {
	return &cards.Env{Theme: theme.Blackwall(), Keys: keys.Default()}
}

// Sprint registered with open sessions for every member, as a running workflow leaves it
func registerProbeSprint(t *testing.T) *Sprint {
	t.Helper()
	snap := probeSnapshot()
	s := newSprint(snap.ID+"-"+t.Name(), snap.Request, snap.Started, nil)
	s.enter(StageBuild, "building")
	for i := range snap.Members {
		m := snap.Members[i]
		m.Live = &harness.Live{Session: session.New()}
		s.putMember(&m)
	}
	tickets := make([]*Ticket, 0, len(snap.Tickets))
	for i := range snap.Tickets {
		tk := snap.Tickets[i]
		tickets = append(tickets, &tk)
	}
	s.setPlan(Plan{Title: "Probe", Summary: "a plan"}, tickets)
	s.say(MsgHandoff, "Linus", "Overwatch", "T2", "done: built two")
	s.say(MsgVerdict, "Overwatch", "Linus", "T2", "accepted: fine")
	register(s)
	t.Cleanup(func() {
		regMu.Lock()
		for i, have := range sprints {
			if have == s {
				sprints = append(sprints[:i], sprints[i+1:]...)
				break
			}
		}
		regMu.Unlock()
	})
	return s
}

// Paints a card into a fresh buffer of the size
func paintInto(c wm.Card, w, h int) *uv.Buffer {
	buf := uv.NewBuffer(w, h)
	c.(wm.Painter).Paint(buf)
	return buf
}

// Arena paints every size without a panic, with the inspector and the panels open, and writes a PNG on request
func TestArenaPaint(t *testing.T) {
	s := registerProbeSprint(t)
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	for i := 0; i < 150; i++ {
		a.sc.update(1.0 / 30)
		if i%30 == 0 {
			a.refresh()
		}
	}
	for _, size := range [][2]int{{4, 2}, {12, 3}, {40, 10}, {80, 20}, {140, 40}, {220, 56}} {
		paintInto(a, size[0], size[1])
		a.moveCamera(1.0 / 30)
	}
	for _, mode := range PixelModes {
		a.cfg.Arena.Pixels = mode
		paintInto(a, 120, 30)
		paintInto(a, 120, 30)
		if !a.staticOK || a.static.W != 120*cellWidth(mode) {
			t.Fatalf("static layer not kept in %s mode: ok %v width %d", mode, a.staticOK, a.static.W)
		}
	}
	a.cfg.Arena.Pixels = "quadrant"
	if _, cmd := a.Update(wm.ActionMsg{ID: ActNext}); cmd != nil {
		cmd()
	}
	if a.sel != s.Snapshot().Members[0].Name {
		t.Fatalf("selected %q after next", a.sel)
	}
	a.Update(wm.ActionMsg{ID: ActInspect})
	a.Update(wm.ActionMsg{ID: ActSteer})
	if !a.inspect || !a.typing {
		t.Fatalf("inspector %v typing %v after steer", a.inspect, a.typing)
	}
	a.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	a.Update(wm.ActionMsg{ID: ActLeave})
	if a.typing {
		t.Fatal("still typing after leave")
	}
	a.Update(wm.ActionMsg{ID: ActBoard})
	for i := 0; i < 150; i++ {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
		if i%30 == 0 {
			a.refresh()
		}
	}
	buf := paintInto(a, 220, 56)
	if a.lay.insp.w == 0 || a.lay.world.w == 0 {
		t.Fatalf("layout %+v", a.lay)
	}
	if len(a.spots) == 0 {
		t.Fatal("no actor was placed on the cells")
	}
	if _, ok := a.chipAt(a.chips[0].r.x, a.chips[0].r.y); !ok {
		t.Fatal("chip of the first member is not under its own cell")
	}
	if cur := a.Cursor(); cur != nil {
		t.Fatal("cursor shown while not typing")
	}
	a.Update(wm.ActionMsg{ID: ActSteer})
	paintInto(a, 220, 56)
	if cur := a.Cursor(); cur == nil || cur.X < a.lay.insp.x {
		t.Fatalf("cursor %+v outside the inspector", cur)
	}
	a.Update(wm.ActionMsg{ID: ActLeave})
	a.Update(wm.ActionMsg{ID: ActBoard})
	a.Update(wm.ActionMsg{ID: ActLog})
	buf = paintInto(a, 220, 56)
	for i := 0; i < 2; i++ {
		a.Update(wm.ActionMsg{ID: ActHelp})
		paintInto(a, 220, 56)
	}
	writeCells(t, buf, "arena.png")
	a.Update(wm.ActionMsg{ID: ActLog})
	a.Update(wm.ActionMsg{ID: ActInspect})
	buf = paintInto(a, 220, 56)
	writeCells(t, buf, "arena-floor.png")
	narrow := paintInto(a, 70, 24)
	writeCells(t, narrow, "arena-narrow.png")
}

// Arena with no sprint keeps a wanderer on the floor and names the command to start one
func TestArenaEmpty(t *testing.T) {
	a := newArena(testEnv()).(*arena)
	a.sprintID = "nothing"
	a.refresh()
	if a.snap != nil || a.sc.lobby == nil {
		t.Fatalf("snapshot %v lobby %v", a.snap, a.sc.lobby)
	}
	for i := 0; i < 30; i++ {
		a.sc.update(0.1)
	}
	paintInto(a, 120, 30)
	if cmd := a.Update2(wm.ActionMsg{ID: ActNext}); cmd == nil {
		t.Fatal("selecting with nobody on the floor said nothing")
	}
}

// Pixels across one cell in a mode
func cellWidth(mode string) int {
	px, _ := vxl.CellModeOf(mode).Pixels()
	return px
}

// Second return of Update, for tests that only read the command
func (a *arena) Update2(msg tea.Msg) tea.Cmd {
	_, cmd := a.Update(msg)
	return cmd
}

// Clicking an actor selects it, clicking it again opens the inspector, dragging the floor turns the camera
func TestArenaMouse(t *testing.T) {
	s := registerProbeSprint(t)
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	for i := 0; i < 90; i++ {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
		a.refresh()
	}
	paintInto(a, 220, 56)
	name := s.Snapshot().Members[3].Name
	sp, ok := a.spots[name]
	if !ok {
		t.Fatalf("no spot for %s", name)
	}
	a.Update(tea.MouseClickMsg{X: sp.x, Y: sp.bottom, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: sp.x, Y: sp.bottom, Button: tea.MouseLeft})
	if a.sel != name {
		t.Fatalf("selected %q after clicking %s", a.sel, name)
	}
	a.Update(tea.MouseClickMsg{X: sp.x, Y: sp.bottom, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: sp.x, Y: sp.bottom, Button: tea.MouseLeft})
	if !a.inspect {
		t.Fatal("second click did not open the inspector")
	}
	yaw := a.rig.yawOff
	a.Update(tea.MouseClickMsg{X: 30, Y: 30, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: 40, Y: 30, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: 40, Y: 30, Button: tea.MouseLeft})
	if a.rig.yawOff == yaw {
		t.Fatal("drag did not turn the camera")
	}
	zoom := a.rig.zoom
	a.Update(tea.MouseWheelMsg{X: 30, Y: 30, Button: tea.MouseWheelUp})
	if a.rig.zoom >= zoom {
		t.Fatal("wheel did not zoom in")
	}
}

// Writes the cells as a PNG under the directory SPRINT_PROBE_DIR names
func writeCells(t *testing.T, buf *uv.Buffer, name string) {
	t.Helper()
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	writeCellsPNG(t, buf, vxl.ModeQuadrant, filepath.Join(dir, name))
}
