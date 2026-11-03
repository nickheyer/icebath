package sprint

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/vxl"
)

// Sprint in the middle of its build with every kind of ticket and member state on the platform
func probeSnapshot() Snapshot {
	now := time.Now()
	members := []Member{
		{Role: RoleDirector, Name: "Fixer"}, {Role: RoleArchitect, Name: "Navigator"}, {Role: RoleReviewer, Name: "Overwatch", Task: "reviewing T2", Ticket: "T2"},
		{Role: RoleBuilder, Name: "Ada", Index: 0, Ticket: "T1", Task: "building T1"}, {Role: RoleBuilder, Name: "Linus", Index: 1, Ticket: "T2", Task: "waiting for the verdict on T2"}, {Role: RoleBuilder, Name: "Grace", Index: 2},
	}
	tickets := []Ticket{
		{ID: "T1", Title: "one", Status: TicketActive, Builder: "Ada", Started: now.Add(-9 * time.Minute)}, {ID: "T2", Title: "two", Status: TicketReview, Builder: "Linus"}, {ID: "T3", Title: "three", Status: TicketDone, Builder: "Grace"},
		{ID: "T4", Title: "four", Status: TicketTodo, DependsOn: []string{"T1"}}, {ID: "T5", Title: "five", Status: TicketTodo, DependsOn: []string{"T3", "T2"}}, {ID: "T6", Title: "six", Status: TicketFailed}, {ID: "T7", Title: "seven", Status: TicketDone, DependsOn: []string{"T6"}},
	}
	msgs := []Message{{Seq: 1, At: now.Add(-8 * time.Minute), Kind: MsgAssign, From: "Overwatch", To: "Ada", Ticket: "T1", Text: "take T1"}, {Seq: 2, At: now.Add(-2 * time.Minute), Kind: MsgHandoff, From: "Linus", To: "Overwatch", Ticket: "T2", Text: "done"}}
	return Snapshot{ID: "sprint-1", Request: "probe", Started: now.Add(-20 * time.Minute), Stage: StageBuild, Status: "building", Members: members, Tickets: tickets, Messages: msgs, Spent: map[string]int{"Ada": 310000, "Linus": 90000}, Usage: map[string]protocol.Usage{"Ada": {Input: 290000, Output: 20000, CacheRead: 1200000}, "Linus": {Input: 80000, Output: 10000, CacheRead: 300000}}}
}

// Harness state of the probe, a runner editing, the overwatch reading and the fixer asking the person
func probeLive() map[string]liveState {
	return map[string]liveState{
		"Ada":       {running: true, tool: "Edit", toolAt: time.Now(), tally: toolTally{reads: 12, edits: 5, shells: 3}, heat: 0.78, since: time.Now().Add(-9 * time.Minute), limit: 90 * time.Minute},
		"Overwatch": {running: true, tool: "Read", toolAt: time.Now(), tally: toolTally{reads: 4}},
		"Fixer":     {asking: true},
		"Grace":     {approval: true},
	}
}

// Renders a scene through the default camera into a frame and the cells of the mode
func renderProbe(sc *scene, cols, rows int, mode vxl.CellMode) (*vxl.Frame, *uv.Buffer) {
	cam, _ := probeView(sc)
	return renderProbeAt(sc, cam, cols, rows, mode)
}

// Renders a scene through a camera into a frame and the cells of the mode, finished as its world asks
func renderProbeAt(sc *scene, cam vxl.Camera, cols, rows int, mode vxl.CellMode) (*vxl.Frame, *uv.Buffer) {
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(cols*px, rows*py)
	_, r := probeView(sc)
	sc.world.light(r, sc, cam.Dist)
	qualityOf("high").apply(r)
	r.Begin(fr, cam)
	fr.Clear(func(x, y int) vxl.RGB { return sc.sky(r.Ray(float64(x)+0.5, float64(y)+0.5)) })
	sc.drawStatic(r)
	sc.drawDynamic(r)
	r.Flush()
	fin := sc.world.finish()
	fr.Outline(fin.outline)
	var post vxl.Post
	fr.Bloom(&post, fin.bloomThreshold, fin.bloomStrength)
	fr.Grade(fin.grade)
	fr.Vignette(fin.vignette)
	buf := uv.NewBuffer(cols, rows)
	fr.Encode(buf, 0, 0, cols, rows, mode, nil)
	return fr, buf
}

// Figures far off draw as markers and blocks and nodes drop their shells, so the far view queues fewer faces
func TestDetailByDistance(t *testing.T) {
	sc := settledScene(probeSnapshot14(), probeLive14(), 8, 150)
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(220*px, 56*py)
	queued := func(dist float64) (int, int) {
		cam, r := probeView(sc)
		cam.Dist = dist
		r.Begin(fr, cam)
		sc.drawDynamic(r)
		n := r.Queued()
		r.Flush()
		full := 0
		for _, a := range sc.actors {
			if a.gone {
				continue
			}
			_, _, depth, ok := r.Project(a.stand().Add(vxl.V(0, a.model.Height()/2, 0)))
			if ok && a.model.Height()*r.ScaleY(depth) >= fullFigurePixels {
				full++
			}
		}
		return n, full
	}
	near, nearFull := queued(float64(sc.lay.w) * 1.2)
	far, farFull := queued(float64(sc.lay.w) * 8)
	if nearFull == 0 || farFull != 0 {
		t.Fatalf("%d figures in full detail near and %d far", nearFull, farFull)
	}
	if far >= near*3/4 {
		t.Fatalf("far view queued %d faces against %d near", far, near)
	}
}

// Conduits of the fourteen ticket plan queue one face per straight run and paint the floor between their nodes
func TestConduitRuns(t *testing.T) {
	sc := settledScene(probeSnapshot14(), probeLive14(), 8, 150)
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(220*px, 56*py)
	fr.Clear(func(x, y int) vxl.RGB { return vxl.RGB{} })
	cam, r := probeView(sc)
	r.Begin(fr, cam)
	runs, cells := 0, 0
	for _, id := range sc.order {
		n := sc.nodes[id]
		for _, dep := range n.deps {
			runs += len(sc.lay.runs[[2]string{id, dep}])
			cells += len(sc.lay.conduits[[2]string{id, dep}])
			sc.drawConduit(r, n, dep)
		}
	}
	r.Flush()
	if runs == 0 || r.Queued() != runs || runs*3 > cells || runs > 48 {
		t.Fatalf("%d conduit faces for %d runs over %d cells", r.Queued(), runs, cells)
	}
	lit := 0
	for _, d := range fr.Depth {
		if d > 0 {
			lit++
		}
	}
	if lit < cells {
		t.Fatalf("conduits over %d cells painted %d pixels", cells, lit)
	}
}

// Renders the probe scenes into cells in every mode, writing PNGs when SPRINT_PROBE_DIR names a directory
func TestProbeRender(t *testing.T) {
	cols, rows := 220, 56
	sc := newScene(3, newBlackwall())
	snap := probeSnapshot()
	live := probeLive()
	cfg := Default()
	for i := range 300 {
		if i%10 == 0 {
			sc.sync(&snap, live, cfg)
		}
		sc.update(1.0 / 30)
	}
	if len(sc.nodes) != 7 || sc.lay.levels["T5"] != 1 || sc.lay.levels["T4"] != 1 || sc.lay.levels["T1"] != 0 {
		t.Fatalf("nodes %d levels %v", len(sc.nodes), sc.lay.levels)
	}
	for _, a := range sc.actors {
		if !a.landed || !sc.lay.onPlatform(a.pos) && a.pos.Y < 1 {
			t.Fatalf("%s is off the platform at %v landed %v", a.name, a.pos, a.landed)
		}
	}
	if ada := sc.byName["Ada"]; ada.duty.kind != dutyNode || ada.state != stateJack && ada.state != stateWalk || ada.heat < 0.7 {
		t.Fatalf("Ada duty %v state %v heat %v", ada.duty.kind, ada.state, ada.heat)
	}
	if grace := sc.byName["Grace"]; grace.frozen < 0.5 {
		t.Fatalf("Grace is not iced while her approval waits: %v", grace.frozen)
	}
	if fixer := sc.byName["Fixer"]; fixer.beacon < 0.5 {
		t.Fatalf("the fixer has no beacon up while asking: %v", fixer.beacon)
	}
	if sc.beamLock != "T2" {
		t.Fatalf("the search beam is on %q, not the node under review", sc.beamLock)
	}
	if n := sc.nodes["T1"]; n.cracks <= 0 || n.shell >= 1 {
		t.Fatalf("edits did not crack the shell of T1: cracks %v shell %v", n.cracks, n.shell)
	}
	fr, buf := renderProbe(sc, cols, rows, vxl.ModeQuadrant)
	drawn := 0
	for _, d := range fr.Depth {
		if d > 0 {
			drawn++
		}
	}
	if drawn < len(fr.Depth)/5 {
		t.Fatalf("only %d of %d pixels drawn", drawn, len(fr.Depth))
	}
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	writePixels(t, fr, filepath.Join(dir, "pixels.png"))
	writeCellsPNG(t, buf, vxl.ModeQuadrant, filepath.Join(dir, "cells.png"))
	_, oct := renderProbe(sc, cols, rows, vxl.ModeOctant)
	writeCellsPNG(t, oct, vxl.ModeOctant, filepath.Join(dir, "cells-octant.png"))
	_, sx := renderProbe(sc, cols, rows, vxl.ModeSextant)
	writeCellsPNG(t, sx, vxl.ModeSextant, filepath.Join(dir, "cells-sextant.png"))
	_, shapes := renderProbe(sc, cols, rows, vxl.ModeShapes)
	writeCellsPNG(t, shapes, vxl.ModeShapes, filepath.Join(dir, "cells-shapes.png"))
	_, all := renderProbe(sc, cols, rows, vxl.ModeAll)
	writeCellsPNG(t, all, vxl.ModeAll, filepath.Join(dir, "cells-all.png"))
	wide := settledScene(probeSnapshot14(), probeLive14(), 8, 300)
	fr14, cells14 := renderProbe(wide, cols, rows, vxl.ModeOctant)
	writePixels(t, fr14, filepath.Join(dir, "fourteen.png"))
	writeCellsPNG(t, cells14, vxl.ModeOctant, filepath.Join(dir, "fourteen-octant.png"))
	close, _ := probeView(wide)
	close.Target, close.Dist = wide.byName["Ada"].stand().Add(vxl.V(0, 2, 0)), 18
	fr14, _ = renderProbeAt(wide, close, cols, rows, vxl.ModeOctant)
	writePixels(t, fr14, filepath.Join(dir, "fourteen-close.png"))
	for _, name := range Worlds {
		if name == "off" {
			continue
		}
		wide.setWorld(worldFor(name))
		for range 30 {
			wide.update(1.0 / 30)
		}
		frw, cellsw := renderProbe(wide, cols, rows, vxl.ModeOctant)
		writePixels(t, frw, filepath.Join(dir, "world-"+name+".png"))
		writeCellsPNG(t, cellsw, vxl.ModeOctant, filepath.Join(dir, "world-"+name+"-cells.png"))
		near, _ := probeView(wide)
		near.Target, near.Dist, near.Pitch = wide.lay.uplink.Add(vxl.V(0, 2, -4)), 22, 0.35
		frn, _ := renderProbeAt(wide, near, cols, rows, vxl.ModeOctant)
		writePixels(t, frn, filepath.Join(dir, "world-"+name+"-near.png"))
	}
	wide.setWorld(newBlackwall())
	snap.Finished, snap.Stage = time.Now(), StageDone
	for i := range 150 {
		if i%10 == 0 {
			sc.sync(&snap, map[string]liveState{}, cfg)
		}
		sc.update(1.0 / 30)
	}
	fr, _ = renderProbe(sc, cols, rows, vxl.ModeQuadrant)
	writePixels(t, fr, filepath.Join(dir, "finale.png"))
	for i := range 400 {
		if i%10 == 0 {
			sc.sync(&snap, map[string]liveState{}, cfg)
		}
		sc.update(1.0 / 30)
	}
	fr, _ = renderProbe(sc, cols, rows, vxl.ModeQuadrant)
	writePixels(t, fr, filepath.Join(dir, "jackout.png"))
	plan := probeSnapshot()
	plan.ID, plan.Stage, plan.Tickets, plan.Messages = "sprint-2", StagePlan, nil, nil
	plan.Members = plan.Members[:2]
	sc.sync(&plan, map[string]liveState{"Navigator": {running: true, tool: "Grep", toolAt: time.Now()}}, cfg)
	for range 90 {
		sc.update(1.0 / 30)
	}
	plan.Tickets = probeSnapshot().Tickets
	for i := range plan.Tickets {
		plan.Tickets[i].Status, plan.Tickets[i].Builder = TicketTodo, ""
	}
	plan.Stage = StageBuild
	sc.sync(&plan, map[string]liveState{}, cfg)
	for range 40 {
		sc.update(1.0 / 30)
	}
	fr, _ = renderProbe(sc, cols, rows, vxl.ModeQuadrant)
	writePixels(t, fr, filepath.Join(dir, "plan.png"))
	sc.sync(nil, nil, cfg)
	for range 200 {
		sc.update(1.0 / 30)
	}
	fr, _ = renderProbe(sc, cols, rows, vxl.ModeQuadrant)
	writePixels(t, fr, filepath.Join(dir, "lobby.png"))
}

// Writes the pixels of a frame, each pixel scaled to its screen shape
func writePixels(t *testing.T, fr *vxl.Frame, path string) {
	t.Helper()
	sw, sh := 4, int(4/fr.PixelAspect)
	px := image.NewRGBA(image.Rect(0, 0, fr.W*sw, fr.H*sh))
	for y := range fr.H {
		for x := range fr.W {
			c := fr.Color[y*fr.W+x].Color()
			for dy := range sh {
				for dx := range sw {
					px.Set(x*sw+dx, y*sh+dy, c)
				}
			}
		}
	}
	savePNG(t, px, path)
}

// Writes the cells as a PNG, eight by sixteen pixels per cell, the glyph of the mode painting its lit pixels
func writeCellsPNG(t *testing.T, buf *uv.Buffer, mode vxl.CellMode, path string) {
	t.Helper()
	px, py := mode.Pixels()
	cols, rows := buf.Width(), buf.Height()
	img := image.NewRGBA(image.Rect(0, 0, cols*8, rows*16))
	for cy := range rows {
		for cx := range cols {
			c := buf.CellAt(cx, cy)
			if c == nil {
				continue
			}
			mask, known := vxl.GlyphMask(c.Content)
			bg, fg := c.Style.Bg, c.Style.Fg
			if bg == nil {
				bg = color.Black
			}
			if fg == nil {
				fg = color.White
			}
			for dy := range 16 {
				for dx := range 8 {
					col := color.Color(bg)
					switch {
					case known:
						bit := dy*py/16*px + dx*px/8
						if mask&(1<<uint(bit)) != 0 {
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
	savePNG(t, img, path)
}

func savePNG(t *testing.T, img image.Image, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// Sprint of fourteen tickets on two levels of ten and four, eight runners on the platform
func probeSnapshot14() Snapshot {
	now := time.Now()
	members := []Member{{Role: RoleDirector, Name: "Fixer"}, {Role: RoleArchitect, Name: "Navigator"}, {Role: RoleReviewer, Name: "Overwatch", Task: "reviewing T2", Ticket: "T2"}}
	statuses := []TicketStatus{TicketActive, TicketReview, TicketDone, TicketActive, TicketTodo, TicketFailed, TicketActive, TicketDone, TicketActive, TicketActive}
	builders := map[int]string{0: "Ada", 1: "Linus", 3: "Grace", 6: "Ken", 8: "Barbara", 9: "Dennis"}
	var tickets []Ticket
	for i, st := range statuses {
		id := "T" + strconv.Itoa(i+1)
		tickets = append(tickets, Ticket{ID: id, Title: id, Status: st, Builder: builders[i], Started: now.Add(-5 * time.Minute)})
	}
	for i := range 4 {
		id := "T" + strconv.Itoa(i+11)
		tickets = append(tickets, Ticket{ID: id, Title: id, Status: TicketTodo, DependsOn: []string{"T" + strconv.Itoa(i*2+1), "T" + strconv.Itoa(i*2+4)}})
	}
	names := []string{"Ada", "Linus", "Grace", "Ken", "Barbara", "Dennis", "Edsger", "Frances"}
	ticketOf := map[string]string{}
	for i, name := range builders {
		ticketOf[name] = tickets[i].ID
	}
	for i, name := range names {
		m := Member{Role: RoleBuilder, Name: name, Index: i, Ticket: ticketOf[name]}
		if m.Ticket != "" {
			m.Task = "building " + m.Ticket
		}
		members = append(members, m)
	}
	return Snapshot{ID: "sprint-14", Request: "probe", Started: now.Add(-20 * time.Minute), Stage: StageBuild, Status: "building", Members: members, Tickets: tickets, Spent: map[string]int{"Ada": 310000}, Usage: map[string]protocol.Usage{"Ada": {Input: 290000, Output: 20000, CacheRead: 1200000}}}
}

// Harness state of the fourteen ticket probe, every runner with a ticket editing or reading
func probeLive14() map[string]liveState {
	live := map[string]liveState{"Overwatch": {running: true, tool: "Read", toolAt: time.Now(), tally: toolTally{reads: 4}}}
	for i, name := range []string{"Ada", "Grace", "Ken", "Barbara", "Dennis"} {
		tool := "Edit"
		if i%2 == 1 {
			tool = "Read"
		}
		live[name] = liveState{running: true, tool: tool, toolAt: time.Now(), tally: toolTally{reads: 3 + i, edits: i}, heat: 0.2 * float64(i), since: time.Now().Add(-4 * time.Minute), limit: 90 * time.Minute}
	}
	return live
}

// Scene of a sprint advanced a count of frames, synced every tenth
func settledScene(snap Snapshot, live map[string]liveState, builders, frames int) *scene {
	sc := newScene(builders, newBlackwall())
	cfg := Default()
	for i := range frames {
		if i%10 == 0 {
			sc.sync(&snap, live, cfg)
		}
		sc.update(1.0 / 30)
	}
	return sc
}

// Camera and raster the probe renders through, the whole platform in view, lit as the world of the scene asks
func probeView(sc *scene) (vxl.Camera, *vxl.Raster) {
	cam := vxl.Camera{Target: sc.lay.center(), Yaw: 0.55, Pitch: 0.58, Dist: float64(sc.lay.w) * 1.6, FOV: 0.85}
	r := &vxl.Raster{}
	sc.world.light(r, sc, cam.Dist)
	return cam, r
}

// Times frames of a scene on a wide octant terminal, the static layer cached as the arena keeps it
func benchFrame(b *testing.B, sc *scene) {
	cols, rows := 220, 56
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(cols*px, rows*py)
	cam, r := probeView(sc)
	qualityOf("high").apply(r)
	r.SetLights(nil)
	r.Begin(fr, cam)
	fr.Clear(func(x, y int) vxl.RGB { return sc.sky(r.Ray(float64(x)+0.5, float64(y)+0.5)) })
	sc.drawStatic(r)
	var layer vxl.Layer
	r.FlushInto(&layer)
	fin := sc.world.finish()
	look := vxl.Finish{Outline: fin.outline, BloomThreshold: fin.bloomThreshold, BloomStrength: fin.bloomStrength, Grade: fin.grade, Vignette: fin.vignette}
	var out vxl.Output
	var prev, in vxl.Changes
	prev.Reset(fr.W, fr.H)
	var enc vxl.Encoder
	buf := uv.NewBuffer(cols, rows)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		sc.update(1.0 / 30)
		layer.Restore(fr, &prev)
		r.Begin(fr, cam)
		sc.drawDynamic(r)
		r.FlushOver(&layer)
		in.CopyFrom(&prev)
		in.Union(r.Changes())
		in.Norm()
		out.Finish(fr, look, &in)
		prev.CopyFrom(r.Changes())
		enc.Encode(&vxl.Frame{W: fr.W, H: fr.H, Color: out.Color, PixelAspect: fr.PixelAspect}, buf, 0, 0, cols, rows, mode, nil, out.Dirt())
	}
}

// Times the static layer of a scene drawn from scratch, as a camera move costs
func benchStatic(b *testing.B, sc *scene) {
	cols, rows := 220, 56
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(cols*px, rows*py)
	cam, r := probeView(sc)
	qualityOf("high").apply(r)
	r.Begin(fr, cam)
	sc.drawStatic(r)
	r.Flush()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.Begin(fr, cam)
		fr.Clear(func(x, y int) vxl.RGB { return sc.sky(r.Ray(float64(x)+0.5, float64(y)+0.5)) })
		sc.drawStatic(r)
		r.Flush()
	}
}

// Frame of the seven ticket probe scene
func BenchmarkFrame(b *testing.B) { benchFrame(b, settledScene(probeSnapshot(), probeLive(), 3, 120)) }

// Static layer of the seven ticket probe scene
func BenchmarkStatic(b *testing.B) { benchStatic(b, settledScene(probeSnapshot(), probeLive(), 3, 1)) }

// Frame of the fourteen ticket probe scene with eight runners
func BenchmarkFrame14(b *testing.B) {
	benchFrame(b, settledScene(probeSnapshot14(), probeLive14(), 8, 120))
}

// Static layer of the fourteen ticket probe scene with eight runners
func BenchmarkStatic14(b *testing.B) {
	benchStatic(b, settledScene(probeSnapshot14(), probeLive14(), 8, 1))
}
