package sprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/anim"
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
	s.say(MsgHandoff, "Linus", "Overwatch", "T2", "done: built two, the second of the probe tickets, with its tests green and the files it owns listed in the handoff")
	s.say(MsgVerdict, "Overwatch", "Linus", "T2", "accepted: fine")
	s.say(MsgChat, "Ada", "Navigator", "T1", "does T1 keep the old encoder entry point")
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

// Hits painted with an id prefix
func (a *arena) hitsWith(prefix string) []hit {
	var out []hit
	for _, h := range a.hits {
		if strings.HasPrefix(h.id, prefix) {
			out = append(out, h)
		}
	}
	return out
}

// Arena paints every size without a panic, with the inspector and the dock sections open and folded, and writes a PNG on request
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
	a.rig.yaw.Kick(3)
	paintInto(a, 120, 30)
	if a.layerOK {
		t.Fatal("lit layer kept while the camera moves")
	}
	for _, v := range []*anim.Value{&a.rig.tx, &a.rig.ty, &a.rig.tz, &a.rig.yaw, &a.rig.pitch, &a.rig.dist} {
		v.Snap()
	}
	for _, mode := range PixelModes {
		a.cfg.Arena.Pixels = mode
		paintInto(a, 120, 30)
		paintInto(a, 120, 30)
		if !a.layerOK || !a.layer.Fits(a.lay.world.w*cellWidth(mode), a.fr.H) || a.lay.world.w >= 120 {
			t.Fatalf("lit layer not kept in %s mode: ok %v floor %d", mode, a.layerOK, a.lay.world.w)
		}
	}
	a.cfg.Arena.Pixels = "shapes"
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
	paintInto(a, 220, 56)
	if a.lay.insp.w == 0 || a.lay.world.w == 0 || a.lay.dock.w != a.dockWidth || a.lay.bar.h != 1 {
		t.Fatalf("layout %+v", a.lay)
	}
	if a.secOpen(secTickets) || a.secBox(secTickets).h != 1 || a.secBox(secLog).h < 3 {
		t.Fatalf("tickets not folded after the tickets key: open %v boxes %+v %+v", a.secOpen(secTickets), a.secBox(secTickets), a.secBox(secLog))
	}
	a.Update(wm.ActionMsg{ID: ActBoard})
	paintInto(a, 220, 56)
	if !a.secOpen(secTickets) || a.secBox(secTickets).h < 3 || a.secBox(secLog).h < 3 {
		t.Fatalf("tickets not opened again: %+v %+v", a.secBox(secTickets), a.secBox(secLog))
	}
	if rows := a.hitsWith("member:"); len(rows) != len(s.Snapshot().Members) {
		t.Fatalf("%d member rows painted for %d members", len(rows), len(s.Snapshot().Members))
	}
	if rows := a.hitsWith("ticket:"); len(rows) < len(s.Snapshot().Tickets) {
		t.Fatalf("%d ticket rows painted for %d tickets", len(rows), len(s.Snapshot().Tickets))
	}
	if len(a.hitsWith("sec:")) != len(sections) {
		t.Fatalf("%d section titles painted", len(a.hitsWith("sec:")))
	}
	if btn := a.hitsWith(ActSidebar); len(btn) != 1 || btn[0].r.y != 0 {
		t.Fatalf("dock button %+v", btn)
	}
	if len(a.spots) == 0 {
		t.Fatal("no actor was placed on the cells")
	}
	for _, h := range a.hitsWith("ticket:") {
		if a.lay.world.contains(h.r.x, h.r.y) && h.r.y < a.lay.world.y {
			t.Fatalf("node label %+v above the floor", h)
		}
	}
	a.dock = false
	paintInto(a, 220, 56)
	if a.lay.dock.w != 0 || len(a.hitsWith("member:")) != 0 || a.lay.world.x != 0 {
		t.Fatalf("dock still laid out after hiding it: %+v", a.lay)
	}
	a.dock = true
	if cur := a.Cursor(); cur != nil {
		t.Fatal("cursor shown while not typing")
	}
	a.Update(wm.ActionMsg{ID: ActSteer})
	paintInto(a, 220, 56)
	if cur := a.Cursor(); cur == nil || cur.X <= a.lay.insp.x {
		t.Fatalf("cursor %+v outside the inspector", cur)
	}
	a.Update(wm.ActionMsg{ID: ActLeave})
	a.Update(wm.ActionMsg{ID: ActLog})
	buf := paintInto(a, 220, 56)
	if a.secOpen(secLog) || a.secBox(secLog).h != 1 {
		t.Fatalf("log not folded after the log key: %+v", a.secBox(secLog))
	}
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
	raw, err := json.Marshal(a.State())
	if err != nil {
		t.Fatal(err)
	}
	b := newArena(testEnv()).(*arena)
	b.dockWidth = 30
	b.secs.Open[secLog.String()] = false
	if err := b.SetState(raw); err != nil {
		t.Fatal(err)
	}
	if b.dockWidth != a.dockWidth || b.secOpen(secLog) != a.secOpen(secLog) || b.rig.preset != a.rig.preset {
		t.Fatalf("state did not carry over: %+v", b.State())
	}
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

// Clicking an actor selects it, clicking it again opens the inspector, dragging the floor turns the camera, edges resize and rows and buttons answer clicks
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
	fx, fy := a.lay.world.x+30, a.lay.world.y+a.lay.world.h-3
	a.Update(tea.MouseClickMsg{X: fx, Y: fy, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: fx + 10, Y: fy, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: fx + 10, Y: fy, Button: tea.MouseLeft})
	if a.rig.yawOff == yaw {
		t.Fatal("drag did not turn the camera")
	}
	zoom := a.rig.zoom
	a.Update(tea.MouseWheelMsg{X: fx, Y: fy, Button: tea.MouseWheelUp})
	if a.rig.zoom >= zoom {
		t.Fatal("wheel did not zoom in")
	}
	paintInto(a, 220, 56)
	edge := a.lay.insp.x
	a.Update(tea.MouseClickMsg{X: edge, Y: 10, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: edge - 8, Y: 10, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: edge - 8, Y: 10, Button: tea.MouseLeft})
	paintInto(a, 220, 56)
	if a.lay.insp.w != a.inspWidth || a.lay.insp.x != edge-8 {
		t.Fatalf("inspector edge drag left it at %+v, width %d", a.lay.insp, a.inspWidth)
	}
	a.Update(wm.ActionMsg{ID: ActInspect})
	paintInto(a, 220, 56)
	edge = a.lay.dock.x + a.lay.dock.w - 1
	a.Update(tea.MouseClickMsg{X: edge, Y: 10, Button: tea.MouseLeft})
	a.Update(tea.MouseMotionMsg{X: edge + 6, Y: 10, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: edge + 6, Y: 10, Button: tea.MouseLeft})
	paintInto(a, 220, 56)
	if a.lay.dock.w != edge+7 || a.dockWidth != edge+7 {
		t.Fatalf("dock edge drag left it at %+v, width %d", a.lay.dock, a.dockWidth)
	}
	other := s.Snapshot().Members[0].Name
	var row hit
	for _, h := range a.hitsWith("member:" + other) {
		row = h
	}
	if row.r.w == 0 {
		t.Fatalf("no dock row for %s", other)
	}
	a.Update(tea.MouseClickMsg{X: row.r.x + 2, Y: row.r.y, Button: tea.MouseLeft})
	if a.sel != other {
		t.Fatalf("selected %q after clicking the dock row of %s", a.sel, other)
	}
	tickets := a.hitsWith("ticket:T2")
	if len(tickets) == 0 {
		t.Fatal("no row for T2")
	}
	a.Update(tea.MouseClickMsg{X: tickets[0].r.x + 2, Y: tickets[0].r.y, Button: tea.MouseLeft})
	if a.selTicket != "T2" || a.sel != "Linus" {
		t.Fatalf("clicking T2 selected ticket %q and member %q", a.selTicket, a.sel)
	}
	a.Update(tea.MouseMotionMsg{X: tickets[0].r.x + 2, Y: tickets[0].r.y})
	if a.hoverTicket != "T2" {
		t.Fatalf("hovering the T2 row marks %q", a.hoverTicket)
	}
	log := a.secBox(secLog)
	a.Update(tea.MouseWheelMsg{X: log.x + 1, Y: log.y + 1, Button: tea.MouseWheelUp})
	if a.secs.Follows(secLog.String()) {
		t.Fatal("wheel up over the log kept it following its end")
	}
	head := a.hitsWith("sec:team")
	if len(head) != 1 {
		t.Fatalf("team title hits %+v", head)
	}
	a.Update(tea.MouseClickMsg{X: head[0].r.x + 3, Y: head[0].r.y, Button: tea.MouseLeft})
	paintInto(a, 220, 56)
	if a.secOpen(secTeam) || len(a.hitsWith("member:")) != 0 {
		t.Fatal("clicking the team title did not fold the team")
	}
	a.Update(tea.MouseClickMsg{X: head[0].r.x + 3, Y: head[0].r.y, Button: tea.MouseLeft})
	labels := a.labels
	btn := a.hitsWith(ActLabels)
	if len(btn) != 1 {
		t.Fatalf("labels button hits %+v", btn)
	}
	a.Update(tea.MouseClickMsg{X: btn[0].r.x + 1, Y: btn[0].r.y, Button: tea.MouseLeft})
	if a.labels == labels {
		t.Fatal("clicking the labels button changed nothing")
	}
	if cmd := a.Update2(wm.ActionMsg{ID: ActSidebar}); cmd == nil {
		t.Fatal("hiding the dock saved no setting")
	}
	paintInto(a, 220, 56)
	if a.dock || a.lay.dock.w != 0 || a.lay.world.x != 0 {
		t.Fatalf("dock still laid out after hiding it: %+v", a.lay)
	}
	a.Update(wm.ActionMsg{ID: ActLog})
	paintInto(a, 220, 56)
	if !a.dock || !a.secOpen(secLog) || a.secBox(secLog).h < 3 {
		t.Fatalf("log key did not bring the dock back with the log open: dock %v log %v box %+v", a.dock, a.secOpen(secLog), a.secBox(secLog))
	}
	narrow := paintInto(a, 80, 24)
	if narrow == nil || a.lay.dock.w != 0 {
		t.Fatalf("dock kept on an 80 column card: %+v", a.lay)
	}
}

// Every dock line fits its width without a cut, the whole text of long titles and log lines wrapping onto more rows
func TestDockWrapsInsteadOfCutting(t *testing.T) {
	s := registerProbeSprint(t)
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	w := a.dockWidth - 3
	for _, b := range []body{a.sprintBody(w), a.teamBody(w), a.ticketBody(w), a.logBody(w)} {
		for _, l := range b.lines {
			if strings.Contains(l, a.env.Theme.Glyphs.Ellipsis) {
				t.Fatalf("dock line cut short: %q", l)
			}
		}
	}
	log := a.logBody(w)
	var handoff []string
	for _, r := range log.rows {
		if r.id == "log:Linus" {
			handoff = log.lines[r.first : r.first+r.n]
		}
	}
	for i := range handoff {
		handoff[i] = strings.TrimSpace(handoff[i])
	}
	if len(handoff) < 2 || !strings.Contains(strings.Join(handoff, " "), "listed in the handoff") {
		t.Fatalf("long log line not wrapped whole: %q", handoff)
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

// Times a whole paint of a wide arena with the dock, the inspector and a long log, as a frame costs late in a sprint
func BenchmarkArenaPaint(b *testing.B) {
	snap := probeSnapshot()
	s := newSprint(snap.ID+"-bench", snap.Request, snap.Started, nil)
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
	for i := range 300 {
		s.say(MsgChat, "Ada", "Linus", "T1", strings.Repeat("words of a long exchange between two builders about an interface ", 1+i%3))
	}
	register(s)
	defer func() {
		regMu.Lock()
		sprints = sprints[:len(sprints)-1]
		regMu.Unlock()
	}()
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	a.sel, a.inspect = "Ada", true
	buf := uv.NewBuffer(220, 56)
	a.Paint(buf)
	for _, v := range []*anim.Value{&a.rig.tx, &a.rig.ty, &a.rig.tz, &a.rig.yaw, &a.rig.pitch, &a.rig.dist} {
		v.Snap()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.sc.update(1.0 / 30)
		a.Paint(buf)
	}
}

// Every world paints at several sizes with the static layer kept, switching worlds rebuilds the platform and the world key cycles through them
func TestArenaWorlds(t *testing.T) {
	s := registerProbeSprint(t)
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	for i := 0; i < 60; i++ {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
	}
	for _, v := range []*anim.Value{&a.rig.tx, &a.rig.ty, &a.rig.tz, &a.rig.yaw, &a.rig.pitch, &a.rig.dist} {
		v.Snap()
	}
	for _, name := range Worlds {
		if name == "off" {
			continue
		}
		a.cfg.Arena.World = name
		a.refresh()
		if a.sc.world.name() != name {
			t.Fatalf("scene is in %q after asking for %q", a.sc.world.name(), name)
		}
		for _, q := range Qualities {
			a.cfg.Arena.Quality = q
			for _, size := range [][2]int{{60, 16}, {120, 30}, {220, 56}} {
				paintInto(a, size[0], size[1])
			}
			paintInto(a, 120, 30)
			if !a.layerOK || a.layerWorld != name {
				t.Fatalf("lit layer not kept in %s at %s quality", name, q)
			}
			if sup := qualityOf(q).super; a.fr.W != a.lay.world.w*cellWidth(a.cfg.Arena.Pixels)*sup {
				t.Fatalf("%s quality drew %d pixels across for %d cells", q, a.fr.W, a.lay.world.w)
			}
		}
		if len(a.spots) == 0 {
			t.Fatalf("no actor placed in %s", name)
		}
		if a.sc.vol == nil || a.sc.chunks == nil {
			t.Fatalf("%s baked no radiance or meshed nothing", name)
		}
	}
	a.cfg.Arena.Quality = "high"
	a.cfg.Arena.World = "blackwall"
	a.refresh()
	for i := range Worlds {
		cmd := a.Update2(wm.ActionMsg{ID: ActWorld})
		if cmd == nil {
			t.Fatal("cycling the world saved nothing")
		}
		if want := Worlds[(i+1)%len(Worlds)]; a.cfg.Arena.World != want {
			t.Fatalf("world %q after cycling, want %q", a.cfg.Arena.World, want)
		}
		paintInto(a, 120, 30)
	}
	if a.cfg.Arena.World != "blackwall" {
		t.Fatalf("world %q after a full cycle", a.cfg.Arena.World)
	}
	if sm := a.sc.shadowStatic; sm != nil {
		t.Fatal("a world without a sun built a shadow map")
	}
	a.cfg.Arena.World = "nightcity"
	a.refresh()
	paintInto(a, 120, 30)
	if a.sc.shadowStatic == nil || a.r.Shadows == nil {
		t.Fatal("night city cast no sun shadows")
	}
	if a.r.AO.Strength <= 0 || a.r.Reflect.Strength <= 0 || a.r.Haze.Density <= 0 {
		t.Fatalf("high quality left passes off: %+v %+v %+v", a.r.AO, a.r.Reflect, a.r.Haze)
	}
	a.cfg.Arena.Quality = "low"
	paintInto(a, 120, 30)
	if a.r.AO.Strength != 0 || a.r.Reflect.Strength != 0 || a.r.Haze.Density != 0 {
		t.Fatalf("low quality left passes on: %+v %+v %+v", a.r.AO, a.r.Reflect, a.r.Haze)
	}
}

// With the world off the floor shows the session windows of the team, clicks pick and type to them, keys reach the typed to one and the cursor lands in its window
func TestArenaOff(t *testing.T) {
	s := registerProbeSprint(t)
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.cfg.Arena.World = "off"
	a.refresh()
	buf := paintInto(a, 220, 56)
	members := s.Snapshot().Members
	if len(a.tiles) != len(members) {
		t.Fatalf("%d session windows for %d members", len(a.tiles), len(members))
	}
	if a.lay.insp.w != 0 || a.lay.world.w == 0 || a.lay.dock.w == 0 {
		t.Fatalf("layout %+v with the world off", a.lay)
	}
	if hits := a.hitsWith("tile:"); len(hits) != len(members) {
		t.Fatalf("%d window headers painted", len(hits))
	}
	for _, id := range []string{ActCamera, ActFollow, ActLabels} {
		if len(a.hitsWith(id)) != 0 {
			t.Fatalf("%s button painted with the world off", id)
		}
	}
	if len(a.hitsWith(ActWorld)) != 1 {
		t.Fatal("no world button painted")
	}
	for _, tl := range a.tiles {
		if tl.r.w < minTileWidth-2 || tl.r.h < minTileHeight {
			t.Fatalf("window %+v too small", tl)
		}
		for _, o := range a.tiles {
			if o.name != tl.name && o.r.contains(tl.r.x, tl.r.y) {
				t.Fatalf("windows %+v and %+v overlap", tl, o)
			}
		}
	}
	writeCells(t, buf, "arena-off.png")
	tl := a.tiles[2]
	a.Update(tea.MouseClickMsg{X: tl.r.x + 2, Y: tl.r.y, Button: tea.MouseLeft})
	if a.sel != tl.name || !a.typing {
		t.Fatalf("clicking the header of %s selected %q typing %v", tl.name, a.sel, a.typing)
	}
	paintInto(a, 220, 56)
	cur := a.Cursor()
	if cur == nil || !tl.r.contains(cur.X, cur.Y) {
		t.Fatalf("cursor %+v outside the window %+v", cur, tl.r)
	}
	if ctx := a.Contexts(); ctx[0] != ContextTyping {
		t.Fatalf("contexts %v while typing", ctx)
	}
	a.Update(tea.KeyPressMsg{Code: 'h', Text: "h"})
	if sess := a.sessionOf(tl.name); sess == nil || sess.Composer.Value() != "h" {
		t.Fatalf("typed key did not reach the prompt of %s", tl.name)
	}
	a.Update(tea.MouseClickMsg{X: tl.r.x + 2, Y: tl.r.y, Button: tea.MouseLeft})
	if a.typing {
		t.Fatal("second header click kept typing")
	}
	other := a.tiles[0]
	a.Update(tea.MouseClickMsg{X: other.r.x + 3, Y: other.r.y + 2, Button: tea.MouseLeft})
	a.Update(tea.MouseReleaseMsg{X: other.r.x + 3, Y: other.r.y + 2, Button: tea.MouseLeft})
	if a.sel != other.name || !a.typing {
		t.Fatalf("clicking the body of %s selected %q typing %v", other.name, a.sel, a.typing)
	}
	a.Update(tea.MouseMotionMsg{X: tl.r.x + 3, Y: tl.r.y + 2})
	if a.hover != tl.name {
		t.Fatalf("hovering %s marks %q", tl.name, a.hover)
	}
	a.Update(wm.ActionMsg{ID: ActLeave})
	if a.typing {
		t.Fatal("leave kept typing")
	}
	a.Update(wm.ActionMsg{ID: ActInspect})
	if !a.typing {
		t.Fatal("inspect did not start typing with the world off")
	}
	a.Update(wm.ActionMsg{ID: ActLeave})
	if !a.Grabs(other.r.x+3, other.r.y+2) == a.Grabs(other.r.x+3, other.r.y) {
		t.Fatal("grabs are the same on the header and the body")
	}
	a.last = time.Now().Add(-50 * time.Millisecond)
	a.painted = time.Now()
	a.step()
	narrow := paintInto(a, 70, 24)
	if narrow == nil || len(a.tiles) != len(members) {
		t.Fatalf("%d windows on a narrow card", len(a.tiles))
	}
	a.cfg.Arena.World = "den"
	a.refresh()
	for range 30 {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
	}
	paintInto(a, 220, 56)
	if a.sc.world.name() != "den" || len(a.spots) == 0 {
		t.Fatalf("world %q with %d spots after turning the world back on", a.sc.world.name(), len(a.spots))
	}
}

// Reports whether a dock section is open
func (a *arena) secOpen(s section) bool { return a.secs.IsOpen(s.String()) }

// Box a dock section took on the last paint
func (a *arena) secBox(s section) rect {
	b, _ := a.secs.Rect(s.String())
	return rect{b.X, b.Y, b.W, b.H}
}
