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
)

// Sprint in the middle of its build with every kind of ticket and member state on the floor
func probeSnapshot() Snapshot {
	now := time.Now()
	members := []Member{
		{Role: RoleDirector, Name: "Director"}, {Role: RoleArchitect, Name: "Architect"}, {Role: RoleReviewer, Name: "Reviewer", Task: "reviewing T2"},
		{Role: RoleBuilder, Name: "Ada", Index: 0, Ticket: "T1", Task: "building T1"}, {Role: RoleBuilder, Name: "Linus", Index: 1, Ticket: "T2", Task: "waiting for the verdict on T2"}, {Role: RoleBuilder, Name: "Grace", Index: 2},
	}
	tickets := []Ticket{
		{ID: "T1", Title: "one", Status: TicketActive, Builder: "Ada"}, {ID: "T2", Title: "two", Status: TicketReview, Builder: "Linus"}, {ID: "T3", Title: "three", Status: TicketDone, Builder: "Grace"},
		{ID: "T4", Title: "four", Status: TicketTodo}, {ID: "T5", Title: "five", Status: TicketTodo}, {ID: "T6", Title: "six", Status: TicketFailed}, {ID: "T7", Title: "seven", Status: TicketDone},
	}
	return Snapshot{ID: "sprint-1", Request: "probe", Started: now.Add(-20 * time.Minute), Stage: StageBuild, Status: "building", Members: members, Tickets: tickets, Messages: []Message{{Seq: 1, Kind: MsgHandoff, From: "Linus", To: "Reviewer", Ticket: "T2", Text: "done"}}}
}

// Renders the probe scene into cells, writing PNGs of the pixels and the cells when SPRINT_PROBE_DIR names a directory
func TestProbeRender(t *testing.T) {
	cols, rows := 220, 56
	sc := newScene(3)
	snap := probeSnapshot()
	live := map[string]liveState{"Ada": {running: true, tool: "Edit", toolAt: time.Now()}, "Reviewer": {running: true, tool: "Read", toolAt: time.Now()}, "Director": {asking: true}}
	cfg := Default()
	for i := 0; i < 240; i++ {
		sc.sync(&snap, live, cfg)
		sc.update(1.0 / 30)
	}
	fr := &Frame{}
	fr.Resize(2*cols, 2*rows)
	fr.Clear(func(x, y int) RGB { return sc.sky(float64(y) / float64(fr.H)) })
	cam := Camera{Target: Vec3{22, 1.5, 14}, Yaw: 0.55, Pitch: 0.58, Dist: 48, FOV: 0.85}
	r := &Raster{FogColor: sc.horizon, FogNear: 40, FogFar: 120}
	r.Begin(fr, cam)
	sc.emit(r)
	drawn := 0
	for _, d := range fr.Depth {
		if d > 0 {
			drawn++
		}
	}
	if drawn < len(fr.Depth)/5 {
		t.Fatalf("only %d of %d pixels drawn", drawn, len(fr.Depth))
	}
	buf := uv.NewBuffer(cols, rows)
	fr.Encode(buf, 0, 0, cols, rows, false, nil)
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	px := image.NewRGBA(image.Rect(0, 0, fr.W*2, fr.H*4))
	for y := 0; y < fr.H; y++ {
		for x := 0; x < fr.W; x++ {
			c := fr.Color[y*fr.W+x].Color()
			for dy := 0; dy < 4; dy++ {
				for dx := 0; dx < 2; dx++ {
					px.Set(x*2+dx, y*4+dy, c)
				}
			}
		}
	}
	cells := image.NewRGBA(image.Rect(0, 0, cols*8, rows*16))
	for cy := 0; cy < rows; cy++ {
		for cx := 0; cx < cols; cx++ {
			c := buf.CellAt(cx, cy)
			mask := 0
			for i, g := range quadrants {
				if g == c.Content {
					mask = i
				}
			}
			bg, fg := c.Style.Bg, c.Style.Fg
			if fg == nil {
				fg = bg
			}
			for dy := 0; dy < 16; dy++ {
				for dx := 0; dx < 8; dx++ {
					bit := 1
					if dx >= 4 {
						bit = 2
					}
					if dy >= 8 {
						bit *= 4
					}
					col := color.Color(bg)
					if mask&bit != 0 {
						col = fg
					}
					cells.Set(cx*8+dx, cy*16+dy, col)
				}
			}
		}
	}
	for name, img := range map[string]image.Image{"pixels.png": px, "cells.png": cells} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}
