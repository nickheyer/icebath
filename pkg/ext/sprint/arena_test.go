package sprint

import (
	"image"
	"image/color"
	"image/png"
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
	s.say(MsgHandoff, "Linus", "Reviewer", "T2", "done: built two")
	s.say(MsgVerdict, "Reviewer", "Linus", "T2", "accepted: fine")
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

// Writes the cells as a PNG, four pixels per cell, under the directory SPRINT_PROBE_DIR names
func writeCells(t *testing.T, buf *uv.Buffer, name string) {
	t.Helper()
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	cols, rows := buf.Width(), buf.Height()
	img := image.NewRGBA(image.Rect(0, 0, cols*8, rows*16))
	for cy := 0; cy < rows; cy++ {
		for cx := 0; cx < cols; cx++ {
			c := buf.CellAt(cx, cy)
			if c == nil {
				continue
			}
			mask := -1
			for i, g := range quadrants {
				if g == c.Content {
					mask = i
				}
			}
			bg, fg := c.Style.Bg, c.Style.Fg
			if bg == nil {
				bg = color.Black
			}
			if fg == nil {
				fg = color.White
			}
			for dy := 0; dy < 16; dy++ {
				for dx := 0; dx < 8; dx++ {
					col := color.Color(bg)
					switch {
					case mask >= 0:
						bit := 1
						if dx >= 4 {
							bit = 2
						}
						if dy >= 8 {
							bit *= 4
						}
						if mask&bit != 0 {
							col = fg
						}
					case c.Content != " " && dx >= 2 && dx < 6 && dy >= 3 && dy < 13 && (dx+dy)%2 == 0:
						col = fg
					}
					img.Set(cx*8+dx, cy*16+dy, col)
				}
			}
		}
	}
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
