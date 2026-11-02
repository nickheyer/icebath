package sprint

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	return Snapshot{ID: "sprint-1", Request: "probe", Started: now.Add(-20 * time.Minute), Stage: StageBuild, Status: "building", Members: members, Tickets: tickets, Messages: msgs, Spent: map[string]int{"Ada": 310000, "Linus": 90000}}
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
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(cols*px, rows*py)
	fr.Clear(func(x, y int) vxl.RGB { return sc.sky(float64(y) / float64(fr.H)) })
	cam := vxl.Camera{Target: sc.lay.center(), Yaw: 0.55, Pitch: 0.58, Dist: float64(sc.lay.w) * 1.6, FOV: 0.85}
	r := &vxl.Raster{FogColor: sc.horizon, FogNear: cam.Dist * 0.95, FogFar: cam.Dist * 3, Sun: sunDir, SunColor: vxl.Hex(0xdfe9f2).Scale(0.72), Ambient: vxl.Hex(0x3a4c66)}
	r.Begin(fr, cam)
	sc.drawStatic(r)
	sc.drawDynamic(r)
	r.Flush()
	var post vxl.Post
	fr.Bloom(&post, 0.62, 0.8)
	fr.Vignette(0.3)
	buf := uv.NewBuffer(cols, rows)
	fr.Encode(buf, 0, 0, cols, rows, mode, nil)
	return fr, buf
}

// Renders the probe scene into cells in every mode, writing PNGs of the pixels and the cells when SPRINT_PROBE_DIR names a directory
func TestProbeRender(t *testing.T) {
	cols, rows := 220, 56
	sc := newScene(3)
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
	masks := map[string]int{}
	for m := range 1 << (px * py) {
		masks[mode.Glyph(m)] = m
	}
	cols, rows := buf.Width(), buf.Height()
	img := image.NewRGBA(image.Rect(0, 0, cols*8, rows*16))
	for cy := range rows {
		for cx := range cols {
			c := buf.CellAt(cx, cy)
			if c == nil {
				continue
			}
			mask, known := masks[c.Content]
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
						if mask&(1<<bit) != 0 {
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

// Frame of the probe scene at the size of a wide terminal in octant mode, the static layer cached as the arena keeps it
func BenchmarkFrame(b *testing.B) {
	cols, rows := 220, 56
	sc := newScene(3)
	snap := probeSnapshot()
	live := probeLive()
	cfg := Default()
	for i := range 120 {
		if i%10 == 0 {
			sc.sync(&snap, live, cfg)
		}
		sc.update(1.0 / 30)
	}
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	static, fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}, &vxl.Frame{PixelAspect: mode.PixelAspect()}
	static.Resize(cols*px, rows*py)
	static.Clear(func(x, y int) vxl.RGB { return sc.sky(float64(y) / float64(static.H)) })
	cam := vxl.Camera{Target: sc.lay.center(), Yaw: 0.55, Pitch: 0.58, Dist: float64(sc.lay.w) * 1.6, FOV: 0.85}
	r := &vxl.Raster{FogColor: sc.horizon, FogNear: cam.Dist * 0.95, FogFar: cam.Dist * 3, Sun: sunDir, SunColor: vxl.Hex(0xdfe9f2).Scale(0.72), Ambient: vxl.Hex(0x3a4c66)}
	r.Begin(static, cam)
	sc.drawStatic(r)
	r.Flush()
	var post vxl.Post
	buf := uv.NewBuffer(cols, rows)
	b.ResetTimer()
	for b.Loop() {
		sc.update(1.0 / 30)
		fr.CopyFrom(static)
		r.Begin(fr, cam)
		sc.drawDynamic(r)
		r.Flush()
		fr.Bloom(&post, 0.62, 0.8)
		fr.Vignette(0.3)
		fr.Encode(buf, 0, 0, cols, rows, mode, nil)
	}
}

// Static layer of the probe scene drawn from scratch, as a camera move costs
func BenchmarkStatic(b *testing.B) {
	cols, rows := 220, 56
	sc := newScene(3)
	snap := probeSnapshot()
	sc.sync(&snap, probeLive(), Default())
	mode := vxl.ModeOctant
	px, py := mode.Pixels()
	fr := &vxl.Frame{PixelAspect: mode.PixelAspect()}
	fr.Resize(cols*px, rows*py)
	cam := vxl.Camera{Target: sc.lay.center(), Yaw: 0.55, Pitch: 0.58, Dist: float64(sc.lay.w) * 1.6, FOV: 0.85}
	r := &vxl.Raster{Sun: sunDir, SunColor: vxl.Hex(0xdfe9f2).Scale(0.72), Ambient: vxl.Hex(0x3a4c66)}
	r.Begin(fr, cam)
	sc.drawStatic(r)
	r.Flush()
	b.ResetTimer()
	for b.Loop() {
		fr.Clear(func(x, y int) vxl.RGB { return sc.sky(float64(y) / float64(fr.H)) })
		r.Begin(fr, cam)
		sc.drawStatic(r)
		r.Flush()
	}
}
