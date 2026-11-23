package tui

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/gfx"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pictures of the transcript drawn as real pixels by the terminal, kitty placed images, sixel or iTerm2, over blank rows the transcript keeps
// for them. A terminal that only takes kitty placeholders keeps them, any other falls back to the half blocks of the gateway.

const (
	// Cell budget of a picture drawn as pixels, larger than the half block budget since its text stays legible
	placedMaxCols, placedMaxRows = 120, 20
	// Pause after the last move of a picture before it is drawn, so scrolling and streaming do not send it again every frame
	pictureSettle = 120 * time.Millisecond
	// Pause after a full repaint before pictures are drawn over it, past the frame that repaints
	pictureRepaint = 60 * time.Millisecond
	// Most pixels one picture carries, kitty taking more than the sixel and iTerm2 encoders keep up with
	placedPixelsKitty, placedPixelsCell = 1.0e6, 0.3e6
	// Pictures whose size and pixels are kept
	placedCacheMax = 24
)

// Picture the terminal draws over blank rows, its layout key and its box, rows counted from the first row of its block or of the window
type imageSpot struct {
	key        imageKey
	row, col   int
	cols, rows int
}

// File of a picture as read here: its natural size, the cell size and box it was laid out at, and its pixels fitted to the box once read
type placedPicture struct {
	w, h       int
	cw, ch     int
	cols, rows int
	rgb        []byte
	pw, ph     int
}

// Screen box a picture is drawn in, its top left cell and its size in cells
type placement struct {
	key              imageKey
	x, y, cols, rows int
}

// Picture sent to a screen box, the surface drawing it and whether the terminal holds it there now
type standing struct {
	surface *gfx.Surface
	drawn   bool
}

// Protocol pictures are drawn with and the cell size their boxes were measured at
type pictureLook struct {
	proto  gfx.Protocol
	cw, ch int
}

// Protocol that draws transcript pictures as pixels here, none where kitty placeholders or half blocks paint them as cells
func (s *inlineImages) placedProtocol() gfx.Protocol {
	if !s.graphics || !s.a.graphicsProbed || terminalGraphicsPick(getenv) == "kitty" {
		return gfx.None
	}
	switch p := s.a.graphics.Protocol(); p {
	case gfx.KittyPlaced, gfx.Sixel, gfx.ITerm:
		return p
	}
	return gfx.None
}

// Reads what the terminal draws before the transcript lays out, every block holding a picture painted again when that changed
func (s *inlineImages) beginFrame() {
	look := pictureLook{proto: s.placedProtocol()}
	if look.proto != gfx.None {
		look.cw, look.ch = s.a.graphics.Cell()
	}
	if look != s.look {
		s.look = look
		s.gen++
	}
}

// Whether transcript pictures are drawn as pixels now
func (s *inlineImages) placing() bool { return s.look.proto != gfx.None }

// Blank rows a picture drawn as pixels keeps in the transcript, nil when its file cannot be read here so the half blocks take it
func (s *inlineImages) pictureRows(key imageKey) []string {
	p := s.pictures[key]
	if p == nil {
		p = &placedPicture{}
		if path := tuigateway.LocalImagePath(key.path); path != "" {
			if f, err := os.Open(path); err == nil {
				if cfg, _, err := image.DecodeConfig(f); err == nil {
					p.w, p.h = cfg.Width, cfg.Height
				}
				f.Close()
			}
		}
		s.pictures[key] = p
		s.pictureOrder = append(s.pictureOrder, key)
		for len(s.pictureOrder) > placedCacheMax {
			delete(s.pictures, s.pictureOrder[0])
			s.pictureOrder = s.pictureOrder[1:]
		}
	}
	if p.w <= 0 || p.h <= 0 {
		return nil
	}
	if p.cols == 0 || p.cw != s.look.cw || p.ch != s.look.ch {
		p.cw, p.ch = s.look.cw, s.look.ch
		p.cols, p.rows = pictureBox(p.w, p.h, key.cols, key.rows, p.cw, p.ch)
		p.rgb = nil
	}
	return slices.Repeat([]string{strings.Repeat(" ", p.cols)}, p.rows)
}

// Cells a picture takes at its natural size or shrunk into the budget with its proportions kept, never grown past its pixels
func pictureBox(w, h, maxCols, maxRows, cw, ch int) (int, int) {
	cw, ch = max(cw, 1), max(ch, 1)
	f := math.Min(1, math.Min(float64(maxCols*cw)/float64(w), float64(maxRows*ch)/float64(h)))
	cols := int(math.Ceil(float64(w) * f / float64(cw)))
	rows := int(math.Ceil(float64(h) * f / float64(ch)))
	return min(max(cols, 1), maxCols), min(max(rows, 1), maxRows)
}

// Screen cell the session content starts at, told by the window manager before it paints
func (s *inlineImages) origin(x, y int) { s.ox, s.oy = x, y }

// Boxes of the pictures in view in content cells, kept free of the background the window gives its cells so a dimmed window never paints over
// a picture the terminal holds
func (s *inlineImages) holes() []wm.Box {
	out := make([]wm.Box, 0, len(s.desired))
	for _, at := range s.desired {
		out = append(out, wm.Box{X: at.x - s.ox, Y: at.y - s.oy, W: at.cols, H: at.rows})
	}
	return out
}

// Takes the pictures wholly inside the transcript window as painted, drawing or taking them down once they hold still. A picture reaching the
// content row limit or past it sits under the completion popup and stays down
func (s *inlineImages) layout(spots []imageSpot, viewTop, limit int) {
	var want []placement
	if s.placing() && !s.closed {
		for _, spot := range spots {
			at := placement{key: spot.key, x: s.ox + spot.col, y: s.oy + viewTop + spot.row, cols: spot.cols, rows: spot.rows}
			if viewTop+spot.row+spot.rows <= limit && s.uncovered(at) {
				want = append(want, at)
			}
		}
	}
	if slices.Equal(want, s.desired) {
		return
	}
	s.desired = want
	s.layoutGen++
	if gen := s.layoutGen; s.a.posts() {
		s.a.post(asyncDoneMsg{run: func() tea.Cmd { return s.moved(gen) }})
	}
}

// Whether nothing else is painted over a screen box: no overlay, the session window in view and unzoomed over, no other window or toast on it
func (s *inlineImages) uncovered(at placement) bool {
	m := s.a.wm
	if m == nil || m.Capturing() {
		return false
	}
	ws := m.Active()
	if ws == nil || m.WorkspaceOf(s.a.card) != ws {
		return false
	}
	if z := ws.Zoomed(); z != nil && z != wm.Card(s.a.card) {
		return false
	}
	box := wm.Box{X: at.x, Y: at.y, W: at.cols, H: at.rows}
	if t, ok := m.ToastRegion(); ok && boxesMeet(t, box) {
		return false
	}
	for _, c := range ws.Cards() {
		if c == wm.Card(s.a.card) {
			continue
		}
		if f, ok := m.ScreenFrameOf(c); ok && boxesMeet(f, box) {
			return false
		}
	}
	return true
}

// Reports whether two boxes share a cell
func boxesMeet(a, b wm.Box) bool {
	return a.X < b.X+b.W && b.X < a.X+a.W && a.Y < b.Y+b.H && b.Y < a.Y+a.H
}

// Waits on a layout that moved
func (s *inlineImages) settleAfter(wait time.Duration, gen int, then func(gen int) tea.Cmd) tea.Cmd {
	return tea.Tick(wait, func(time.Time) tea.Msg { return asyncDoneMsg{run: func() tea.Cmd { return then(gen) }} })
}

// Takes down at once every picture no longer wanted where it stands, a full repaint clearing the sixel and iTerm2 ones before the text moving
// over them leaves pieces behind, then waits for the layout to hold still before drawing
func (s *inlineImages) moved(gen int) tea.Cmd {
	drop := s.takeDown(func(at placement, _ *standing) bool { return !slices.Contains(s.desired, at) })
	wait := s.settleAfter(pictureSettle, gen, s.settle)
	if !s.repaint {
		return tea.Batch(drop, wait)
	}
	s.repaint = false
	for _, st := range s.shown {
		st.drawn = false
	}
	return tea.Batch(drop, tea.ClearScreen, wait)
}

// Removes the pictures the test picks from the screen, a kitty image deleted outright and a sixel or iTerm2 one, which is part of the cells,
// left for a full repaint to clear
func (s *inlineImages) takeDown(gone func(placement, *standing) bool) tea.Cmd {
	var out strings.Builder
	for at, st := range s.shown {
		if !gone(at, st) {
			continue
		}
		text := st.surface.Release()
		if st.drawn {
			if st.surface.Protocol() == gfx.KittyPlaced {
				out.WriteString(text)
			} else {
				s.repaint = true
			}
		}
		delete(s.shown, at)
	}
	if out.Len() == 0 {
		return nil
	}
	return tea.Raw(out.String())
}

// Draws the wanted pictures once the layout held still, after a full repaint when a sixel or iTerm2 picture had to go
func (s *inlineImages) settle(gen int) tea.Cmd {
	if gen != s.layoutGen {
		return nil
	}
	drop := s.takeDown(func(at placement, st *standing) bool {
		return st.surface.Protocol() != s.look.proto || !slices.Contains(s.desired, at)
	})
	if !s.repaint {
		return tea.Batch(drop, s.draw(gen))
	}
	s.repaint = false
	for _, st := range s.shown {
		st.drawn = false
	}
	return tea.Batch(drop, tea.ClearScreen, s.settleAfter(pictureRepaint, gen, s.draw))
}

// Sends every wanted picture the terminal does not hold, each read and packed off the update loop and placed if the layout still wants it
func (s *inlineImages) draw(gen int) tea.Cmd {
	if gen != s.layoutGen || !s.placing() {
		return nil
	}
	var cmds []tea.Cmd
	for _, at := range s.desired {
		st := s.shown[at]
		if st != nil && st.drawn {
			continue
		}
		p := s.pictures[at.key]
		if p == nil || p.cols != at.cols || p.rows != at.rows {
			continue
		}
		if st == nil || st.surface.Protocol() != s.look.proto {
			st = &standing{surface: s.a.graphics.NewSurface(s.look.proto)}
			s.shown[at] = st
		}
		cmds = append(cmds, s.pack(gen, at, st.surface, p))
	}
	return tea.Batch(cmds...)
}

// Reads the picture fitted to its box the first time and packs it for the surface beside the update loop
func (s *inlineImages) pack(gen int, at placement, surface *gfx.Surface, p *placedPicture) tea.Cmd {
	w, h := fitPixels(at.cols*max(p.cw, 1), at.rows*max(p.ch, 1), surface.Protocol())
	rgb := p.rgb
	if p.pw != w || p.ph != h {
		rgb = nil
	}
	path, bg := tuigateway.LocalImagePath(at.key.path), s.a.toolkit.Surface
	return func() tea.Msg {
		if rgb == nil {
			fitted, err := fitPicture(path, w, h, bg)
			if err != nil {
				return asyncDoneMsg{run: func() tea.Cmd { return nil }}
			}
			rgb = fitted
		}
		packet := surface.Pack(rgb, w, h, at.cols, at.rows)
		return asyncDoneMsg{run: func() tea.Cmd { return s.place(gen, at, surface, packet, rgb, w, h) }}
	}
}

// Writes a packed picture to the terminal where its box stands, keeping its pixels for the next time it is drawn
func (s *inlineImages) place(gen int, at placement, surface *gfx.Surface, packet *gfx.Packet, rgb []byte, w, h int) tea.Cmd {
	if p := s.pictures[at.key]; p != nil && p.cols == at.cols && p.rows == at.rows {
		p.rgb, p.pw, p.ph = rgb, w, h
	}
	st := s.shown[at]
	if gen != s.layoutGen || st == nil || st.surface != surface || packet == nil || !slices.Contains(s.desired, at) {
		return nil
	}
	text := surface.Emit(packet, at.x, at.y, time.Now())
	if text == "" {
		return nil
	}
	st.drawn = true
	return tea.Raw(text)
}

// Forgets every picture the terminal holds, returning the text that deletes the kitty ones, the sixel and iTerm2 ones going with the cells
func (s *inlineImages) dropAll() tea.Cmd {
	var out strings.Builder
	for at, st := range s.shown {
		text := st.surface.Release()
		if st.drawn && st.surface.Protocol() == gfx.KittyPlaced {
			out.WriteString(text)
		}
		delete(s.shown, at)
	}
	s.repaint = false
	s.layoutGen++
	if out.Len() == 0 {
		return nil
	}
	return tea.Raw(out.String())
}

// Forgets what the terminal holds once the screen was cleared or resized, every wanted picture drawn again once things hold still
func (s *inlineImages) redraw() tea.Cmd {
	drop := s.dropAll()
	return tea.Batch(drop, s.settleAfter(pictureSettle, s.layoutGen, s.settle))
}

// Deletes the pictures the terminal holds as the screen is left, none drawn again
func (s *inlineImages) release() tea.Cmd {
	s.closed, s.desired = true, nil
	return s.dropAll()
}

// Pixels a box of that many pixels is drawn at, shrunk with its proportions kept to what the protocol carries
func fitPixels(w, h int, proto gfx.Protocol) (int, int) {
	limit := placedPixelsCell
	if proto == gfx.KittyPlaced {
		limit = placedPixelsKitty
	}
	w, h = max(w, 1), max(h, 1)
	f := math.Min(1, math.Sqrt(limit/float64(w*h)))
	return max(int(float64(w)*f), 1), max(int(float64(h)*f), 1)
}

// Three byte pixels of the image file scaled into a w by h box from its top left corner, proportions kept and the rest of the box in bg
func fitPicture(path string, w, h int, bg color.Color) ([]byte, error) {
	if path == "" {
		return nil, errors.New("no local image")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, errors.New("empty image")
	}
	scale := math.Min(float64(w)/float64(b.Dx()), float64(h)/float64(b.Dy()))
	dw := min(max(int(math.Round(float64(b.Dx())*scale)), 1), w)
	dh := min(max(int(math.Round(float64(b.Dy())*scale)), 1), h)
	if bg == nil {
		bg = color.Black
	}
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(canvas, image.Rect(0, 0, dw, dh), src, b, xdraw.Over, nil)
	rgb := make([]byte, w*h*3)
	for i, j := 0, 0; i+3 < len(canvas.Pix) && j+2 < len(rgb); i, j = i+4, j+3 {
		rgb[j], rgb[j+1], rgb[j+2] = canvas.Pix[i], canvas.Pix[i+1], canvas.Pix[i+2]
	}
	return rgb, nil
}

// Clears and repaints the whole screen, the pictures the terminal held drawn again after it
func (s *tuiStore) clearScreen() tea.Cmd {
	if s.screenCleared == nil {
		return tea.ClearScreen
	}
	return tea.Batch(tea.ClearScreen, s.screenCleared())
}
