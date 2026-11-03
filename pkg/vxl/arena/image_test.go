//go:build ignore

package arena

import (
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Terminal of a kind as the probe would find it, kitty graphics answered and the cells measured
func probeTerminal(env map[string]string, kittyOK bool) *gfx.Terminal {
	term := gfx.NewWith(func(k string) string { return env[k] })
	if kittyOK {
		term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: 31}, Payload: []byte("OK")})
	}
	term.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	return term
}

// Raw texts the worker and the card sent to the terminal
func rawSent(a *arena) []string {
	in := inboxes[a.env]
	var out []string
	for _, msg := range in.msgs {
		if r, ok := msg.(tea.RawMsg); ok {
			out = append(out, r.Msg.(string))
		}
	}
	return out
}

// In kitty the floor goes out as pixels with placeholder cells naming the image, one frame in flight until the terminal acknowledges it
func TestArenaImage(t *testing.T) {
	term := probeTerminal(map[string]string{"KITTY_WINDOW_ID": "1", "TERM": "xterm-kitty"}, true)
	if term.Protocol() != gfx.KittyCells {
		t.Fatalf("protocol %s", term.Protocol())
	}
	env := testEnv()
	env.Graphics = term
	s := registerProbeSprint(t)
	a := newArena(env).(*arena)
	a.sprintID = s.ID
	a.cfg.Arena.Display = "auto"
	a.refresh()
	a.Origin(3, 2)
	paintInto(a, 180, 48)
	if a.proto != gfx.KittyCells || a.surface == nil {
		t.Fatalf("surface %v protocol %s under auto in kitty", a.surface != nil, a.proto)
	}
	a.advance(90)
	a.settleCamera()
	a.advance(1)
	f := a.frame
	if f == nil || f.packet == nil || f.cells != nil {
		t.Fatal("frame carries no packet in image mode")
	}
	wb := a.lay.world
	if a.stage.fr.W > wb.w*10 || a.stage.fr.H > wb.h*20 || a.stage.fr.W < 64 {
		t.Fatalf("frame %dx%d for %dx%d cells", a.stage.fr.W, a.stage.fr.H, wb.w, wb.h)
	}
	before := len(rawSent(a))
	paintInto(a, 180, 48)
	paintInto(a, 180, 48)
	sent := rawSent(a)
	if len(sent) != before+1 {
		t.Fatalf("%d transmissions for two paints of one frame", len(sent)-before)
	}
	if !strings.Contains(sent[len(sent)-1], "\x1b_G") || !strings.Contains(sent[len(sent)-1], "U=1") {
		t.Fatalf("transmission %q is not a kitty placeholder image", sent[len(sent)-1][:min(len(sent[len(sent)-1]), 80)])
	}
	if a.deliver() != 0 {
		t.Fatal("a frame arrived without the worker drawing one")
	}
	a.stage.drain()
	a.stage.tick(time.Now().Add(100 * time.Millisecond))
	if a.deliver() != 0 {
		t.Fatal("the worker drew a frame while the last one waits for the terminal")
	}
	id := uint32(0)
	for _, raw := range sent {
		for _, part := range strings.Split(raw, ",") {
			if n, ok := strings.CutPrefix(part, "i="); ok {
				var v uint32
				for _, ch := range n {
					if ch < '0' || ch > '9' {
						break
					}
					v = v*10 + uint32(ch-'0')
				}
				id = v
			}
		}
	}
	if id == 0 {
		t.Fatalf("no image id in %q", sent[len(sent)-1][:min(len(sent[len(sent)-1]), 80)])
	}
	term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: int(id)}, Payload: []byte("OK")})
	buf := paintInto(a, 180, 48)
	c := buf.CellAt(wb.x+wb.w/2, wb.y+wb.h/2)
	if !gfx.Placeholder(c) {
		t.Fatalf("cell in the floor is %q after the acknowledgement", c.Content)
	}
	labels := 0
	for y := wb.y; y < wb.y+wb.h; y++ {
		for x := wb.x; x < wb.x+wb.w; x++ {
			if cell := buf.CellAt(x, y); cell != nil && !gfx.Placeholder(cell) && cell.Content != " " {
				labels++
			}
		}
	}
	if labels == 0 {
		t.Fatal("no label cell over the image")
	}
	a.advance(1)
	if a.frame.gen <= f.gen || a.frame.packet == nil {
		t.Fatal("no new frame once the terminal acknowledged the last")
	}
	a.cfg.Arena.Display = "glyphs"
	paintInto(a, 180, 48)
	if a.surface != nil || a.proto != gfx.None {
		t.Fatal("surface kept after switching to glyphs")
	}
	if rel := rawSent(a); !strings.Contains(rel[len(rel)-1], "a=d") {
		t.Fatalf("switching to glyphs did not release the image, last raw %q", rel[len(rel)-1][:min(len(rel[len(rel)-1]), 60)])
	}
	a.advance(1)
	buf = paintInto(a, 180, 48)
	if a.frame.cells == nil || gfx.Placeholder(buf.CellAt(wb.x+wb.w/2, wb.y+wb.h/2)) {
		t.Fatal("glyph frame not painted after leaving image mode")
	}
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	fr := &a.stage.fr
	img := image.NewRGBA(image.Rect(0, 0, fr.W, fr.H))
	for i := range fr.Color {
		img.Set(i%fr.W, i/fr.W, fr.Color[i].Color())
	}
	fh, err := os.Create(filepath.Join(dir, "arena-image.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if err := png.Encode(fh, img); err != nil {
		t.Fatal(err)
	}
	writeCellsPNG(t, buf, vxl.ModeShapes, filepath.Join(dir, "arena-shapes.png"))
}

// In konsole auto draws glyphs, image draws placed kitty frames without placeholders, and vscode draws glyphs unless forced to iTerm2 images
func TestArenaDisplayChoice(t *testing.T) {
	konsole := probeTerminal(map[string]string{"KONSOLE_VERSION": "260801", "TERM": "xterm-256color"}, true)
	if konsole.Protocol() != gfx.KittyPlaced {
		t.Fatalf("konsole protocol %s", konsole.Protocol())
	}
	env := testEnv()
	env.Graphics = konsole
	s := registerProbeSprint(t)
	a := newArena(env).(*arena)
	a.sprintID = s.ID
	a.refresh()
	paintInto(a, 180, 48)
	if a.proto != gfx.None {
		t.Fatalf("auto picked %s in konsole", a.proto)
	}
	a.cfg.Arena.Display = "image"
	paintInto(a, 180, 48)
	if a.proto != gfx.KittyPlaced {
		t.Fatalf("image picked %s in konsole", a.proto)
	}
	a.advance(30)
	a.settleCamera()
	a.advance(1)
	buf := paintInto(a, 180, 48)
	sent := rawSent(a)
	last := sent[len(sent)-1]
	if !strings.Contains(last, "\x1b_G") || strings.Contains(last, "U=1") || strings.Contains(last, "t=s") || !strings.Contains(last, "C=1") {
		t.Fatalf("konsole transmission %q", last[:min(len(last), 100)])
	}
	wb := a.lay.world
	if c := buf.CellAt(wb.x+wb.w/2, wb.y+wb.h/2); gfx.Placeholder(c) || c.Style.Bg != nil {
		t.Fatalf("cell under a placed image is %q with background %v", c.Content, c.Style.Bg)
	}
	vscode := probeTerminal(map[string]string{"TERM_PROGRAM": "vscode", "TERM": "xterm-256color"}, false)
	env2 := testEnv()
	env2.Graphics = vscode
	b := newArena(env2).(*arena)
	b.sprintID = s.ID
	b.refresh()
	paintInto(b, 180, 48)
	if b.proto != gfx.None {
		t.Fatalf("auto picked %s in vscode", b.proto)
	}
	b.cfg.Arena.Display = "image"
	paintInto(b, 180, 48)
	if b.proto != gfx.ITerm {
		t.Fatalf("image picked %s in vscode", b.proto)
	}
	b.cfg.Arena.World = "off"
	paintInto(b, 180, 48)
	if b.proto != gfx.None || b.surface != nil {
		t.Fatal("surface kept with the world off")
	}
}
