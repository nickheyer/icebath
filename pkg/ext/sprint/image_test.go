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
	"github.com/charmbracelet/x/ansi/kitty"

	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// In a terminal that draws kitty graphics the floor goes out as pixels, the cells under it naming the image, and the PNG lands in the probe directory
func TestArenaImage(t *testing.T) {
	t.Setenv("KITTY_WINDOW_ID", "1")
	t.Setenv("TMUX", "")
	term := gfx.New()
	term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: 31}, Payload: []byte("OK")})
	term.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	if term.Protocol() != gfx.KittyCells {
		t.Fatalf("protocol %s", term.Protocol())
	}
	var sent []tea.Msg
	env := testEnv()
	env.Graphics = term
	env.Send = func(msg tea.Msg) { sent = append(sent, msg) }
	s := registerProbeSprint(t)
	a := newArena(env).(*arena)
	a.sprintID = s.ID
	a.cfg.Arena.Pixels = "image"
	a.refresh()
	a.live = probeLive()
	a.sc.sync(a.snap, a.live, a.cfg)
	for range 90 {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
	}
	for _, v := range []*anim.Value{&a.rig.tx, &a.rig.ty, &a.rig.tz, &a.rig.yaw, &a.rig.pitch, &a.rig.dist} {
		v.Snap()
	}
	a.Origin(3, 2)
	paintInto(a, 180, 48)
	paintInto(a, 180, 48)
	if len(sent) != 1 {
		t.Fatalf("%d images sent for two paints of one frame", len(sent))
	}
	if _, ok := sent[0].(tea.RawMsg); !ok {
		t.Fatalf("sent %T", sent[0])
	}
	a.sc.update(1.0 / 30)
	buf := paintInto(a, 180, 48)
	if len(sent) != 2 {
		t.Fatalf("%d images sent once the scene moved", len(sent))
	}
	wb := a.lay.world
	px, py, _ := a.imagePixels(wb.w, wb.h)
	if a.fr.W != wb.w*px || a.fr.H != wb.h*py || px > 10 || py > 20 || px < 2 {
		t.Fatalf("frame %dx%d for %dx%d cells at %dx%d", a.fr.W, a.fr.H, wb.w, wb.h, px, py)
	}
	c := buf.CellAt(wb.x+wb.w/2, wb.y+wb.h/2)
	if !gfx.Placeholder(c) {
		t.Fatalf("cell in the floor is %q", c.Content)
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
	defer a.releaseImage()
	dir := os.Getenv("SPRINT_PROBE_DIR")
	if dir == "" {
		return
	}
	img := image.NewRGBA(image.Rect(0, 0, a.fr.W, a.fr.H))
	for i := range a.fr.Color {
		img.Set(i%a.fr.W, i/a.fr.W, color.RGBA{a.rgb[i*3], a.rgb[i*3+1], a.rgb[i*3+2], 255})
	}
	f, err := os.Create(filepath.Join(dir, "arena-image.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	a.cfg.Arena.Pixels = "shapes"
	paintInto(a, 180, 48)
	buf = paintInto(a, 180, 48)
	writeCellsPNG(t, buf, vxl.ModeShapes, filepath.Join(dir, "arena-shapes.png"))
}
