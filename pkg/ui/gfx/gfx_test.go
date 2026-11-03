package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Frame of w by h pixels ramping red across and green down
func ramp(w, h int) []byte {
	out := make([]byte, w*h*3)
	for y := range h {
		for x := range w {
			i := (y*w + x) * 3
			out[i], out[i+1], out[i+2] = byte(x*255/max(w-1, 1)), byte(y*255/max(h-1, 1)), 40
		}
	}
	return out
}

// A kitty terminal that draws placeholders gets chunked transmissions and cells naming the image one frame behind
func TestKittyCells(t *testing.T) {
	term := &Terminal{program: "kitty", placeholders: true, nextID: 0x2c0000}
	if !term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: queryID}, Payload: []byte("OK")}) || term.Protocol() != KittyCells {
		t.Fatalf("protocol %s after the kitty answer", term.Protocol())
	}
	term.Update(uv.CellSizeEvent{Width: 10, Height: 20})
	if w, h := term.Cell(); w != 10 || h != 20 {
		t.Fatalf("cell %dx%d", w, h)
	}
	s := term.NewSurface()
	w, h := 64, 40
	out := s.Encode(ramp(w, h), w, h, 3, 2, 8, 2)
	chunks := strings.Split(out, "\x1b\\")
	if len(chunks) < 2 {
		t.Fatalf("one transmission in %q", out)
	}
	first := chunks[0]
	for _, want := range []string{"a=T", "f=24", "o=z", "s=64", "v=40", "c=8", "r=2", "U=1", "q=2"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first chunk %q lacks %s", first, want)
		}
	}
	var payload []byte
	for _, c := range chunks {
		if c == "" {
			continue
		}
		_, data, ok := strings.Cut(c, ";")
		if !ok {
			t.Fatalf("chunk without a payload %q", c)
		}
		if len(data) > kittyChunk || len(data)%4 != 0 {
			t.Fatalf("chunk of %d base64 bytes", len(data))
		}
		payload = append(payload, data...)
	}
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(payload)))
	n, err := base64.StdEncoding.Decode(raw, payload)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw[:n]))
	if err != nil {
		t.Fatal(err)
	}
	pixels, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(pixels, ramp(w, h)) {
		t.Fatalf("pixels did not survive the transmission, %v", err)
	}
	buf := uv.NewBuffer(12, 5)
	s.Paint(buf, 3, 2, 8, 2, nil)
	if c := buf.CellAt(3, 2); c.Content != " " {
		t.Fatalf("first frame painted %q before any image reached the terminal", c.Content)
	}
	s.Encode(ramp(w, h), w, h, 3, 2, 8, 2)
	s.Paint(buf, 3, 2, 8, 2, nil)
	c := buf.CellAt(4, 3)
	if !Placeholder(c) || c.Width != 1 {
		t.Fatalf("cell %q width %d is no placeholder", c.Content, c.Width)
	}
	want := string(kitty.Placeholder) + string(kitty.Diacritic(1)) + string(kitty.Diacritic(1))
	if c.Content != want {
		t.Fatalf("cell at row 1 col 1 holds %q, want %q", c.Content, want)
	}
	r, g, b, _ := c.Style.Fg.RGBA()
	id := uint32(r>>8)<<16 | uint32(g>>8)<<8 | uint32(b>>8)
	if id != s.ids[0] {
		t.Fatalf("cells name image %d, want the first transmitted %d", id, s.ids[0])
	}
	if rel := s.Release(); !strings.Contains(rel, "a=d") || !strings.Contains(rel, "d=I") {
		t.Fatalf("release %q", rel)
	}
}

// Without placeholders kitty graphics are placed at the cursor under the text and the cells stay on the default background
func TestKittyPlaced(t *testing.T) {
	term := &Terminal{program: "wezterm", nextID: 10}
	term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: queryID}, Payload: []byte("OK")})
	if term.Protocol() != KittyPlaced {
		t.Fatalf("protocol %s", term.Protocol())
	}
	s := term.NewSurface()
	out := s.Encode(ramp(8, 8), 8, 8, 5, 4, 2, 1)
	if !strings.HasPrefix(out, "\x1b7\x1b[5;6H") || !strings.HasSuffix(out, "\x1b8") || !strings.Contains(out, "C=1") || !strings.Contains(out, "z=-1") {
		t.Fatalf("placed transmission %q", out)
	}
	buf := uv.NewBuffer(8, 6)
	s.Paint(buf, 5, 4, 2, 1, nil)
	if c := buf.CellAt(6, 4); c.Content != " " || c.Style.Bg != nil {
		t.Fatalf("cell under a placed image is %q with background %v", c.Content, c.Style.Bg)
	}
}

// A sixel terminal gets a well formed bitmap of the fixed palette placed at the cursor, once its cells are measured
func TestSixel(t *testing.T) {
	term := &Terminal{program: "xterm", nextID: 10}
	term.Update(uv.PrimaryDeviceAttributesEvent([]int{62, 4, 22}))
	if term.Protocol() != None {
		t.Fatalf("protocol %s before the cell size is known", term.Protocol())
	}
	term.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	term.Update(uv.PixelSizeEvent{Width: 1000, Height: 1000})
	if w, h := term.Cell(); w != 10 || h != 20 || term.Protocol() != Sixel {
		t.Fatalf("cell %dx%d protocol %s", w, h, term.Protocol())
	}
	s := term.NewSurface()
	out := s.Encode(ramp(20, 13), 20, 13, 0, 0, 2, 1)
	if !strings.Contains(out, "\x1bP0;1;q\"1;1;20;13#0;2;0;0;0") || !strings.HasSuffix(out, "\x1b\\\x1b8") {
		t.Fatalf("sixel %q", out[:min(len(out), 80)])
	}
	bands := strings.Count(out, "-")
	if bands != 3 {
		t.Fatalf("%d bands for 13 rows", bands)
	}
	if !strings.Contains(out, "#") || strings.Contains(out, "\x1b\\\x1b\\") {
		t.Fatalf("sixel %q", out)
	}
	if rel := s.Release(); !strings.Contains(rel, "\x1b[H  ") {
		t.Fatalf("release %q", rel)
	}
}

// An iTerm2 terminal gets a PNG sized in cells that leaves the cursor where it was
func TestITerm(t *testing.T) {
	term := &Terminal{program: "iterm.app", iterm: true, nextID: 10}
	if term.Protocol() != ITerm {
		t.Fatalf("protocol %s", term.Protocol())
	}
	s := term.NewSurface()
	out := s.Encode(ramp(16, 16), 16, 16, 2, 1, 4, 2)
	if !strings.Contains(out, "\x1b]1337;File=inline=1;size=") || !strings.Contains(out, "width=4;height=2;preserveAspectRatio=0;doNotMoveCursor=1:") || !strings.HasSuffix(out, "\x07\x1b8") {
		t.Fatalf("iterm %q", out[:min(len(out), 120)])
	}
}

// Under tmux only placeholder cells are drawn, the transmission wrapped for the terminal beneath
func TestTmux(t *testing.T) {
	term := &Terminal{program: "kitty", placeholders: true, kitty: true, tmux: true, nextID: 10}
	if term.Protocol() != KittyCells {
		t.Fatalf("protocol %s", term.Protocol())
	}
	s := term.NewSurface()
	out := s.Encode(ramp(4, 4), 4, 4, 0, 0, 1, 1)
	if !strings.HasPrefix(out, "\x1bPtmux;\x1b\x1b_G") || !strings.HasSuffix(out, "\x1b\\") {
		t.Fatalf("tmux wrap %q", out[:min(len(out), 40)])
	}
	plain := &Terminal{program: "xterm", sixel: true, tmux: true, cellW: 8, cellH: 16}
	if plain.Protocol() != None {
		t.Fatalf("sixel offered under tmux")
	}
}

// Frame of w by h pixels of a lit scene with grain, as a floor frame compresses
func grainy(w, h int) []byte {
	out := make([]byte, w*h*3)
	seed := uint32(7)
	for y := range h {
		for x := range w {
			i := (y*w + x) * 3
			seed = seed*1664525 + 1013904223
			n := byte(seed >> 28)
			out[i], out[i+1], out[i+2] = byte(x*200/max(w-1, 1))+n, byte(y*160/max(h-1, 1))+n, 40+n
		}
	}
	return out
}

func BenchmarkKittyEncode(b *testing.B) {
	term := &Terminal{program: "kitty", placeholders: true, kitty: true, nextID: 10}
	s := term.NewSurface()
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Encode(rgb, w, h, 0, 0, 136, 48)
	}
	b.ReportMetric(float64(len(s.out.Bytes()))/1e6, "MB/frame")
}

func BenchmarkSixelEncode(b *testing.B) {
	term := &Terminal{program: "xterm", sixel: true, cellW: 10, cellH: 20, nextID: 10}
	s := term.NewSurface()
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Encode(rgb, w, h, 0, 0, 136, 48)
	}
	b.ReportMetric(float64(len(s.out.Bytes()))/1e6, "MB/frame")
}

func BenchmarkITermEncode(b *testing.B) {
	term := &Terminal{program: "iterm.app", iterm: true, nextID: 10}
	s := term.NewSurface()
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Encode(rgb, w, h, 0, 0, 136, 48)
	}
	b.ReportMetric(float64(len(s.out.Bytes()))/1e6, "MB/frame")
}

// On the same machine kitty graphics go through a shared memory object the transmission names, one the terminal would unlink
func TestKittyShared(t *testing.T) {
	if !shmWorks() {
		t.Skip("no shared memory objects here")
	}
	term := &Terminal{program: "kitty", placeholders: true, kitty: true, shm: true, nextID: firstID}
	s := term.NewSurface()
	out := s.Encode(ramp(8, 4), 8, 4, 0, 0, 2, 1)
	if !strings.Contains(out, "t=s") || !strings.Contains(out, "S=96") || strings.Contains(out, "o=z") {
		t.Fatalf("shared transmission %q", out)
	}
	_, payload, _ := strings.Cut(out, ";")
	name, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(payload, "\x1b\\"))
	if err != nil || !strings.HasPrefix(string(name), "/icebath-") {
		t.Fatalf("object name %q %v", name, err)
	}
	data, err := os.ReadFile(shmDir + strings.TrimPrefix(string(name), "/"))
	if err != nil || !bytes.Equal(data, ramp(8, 4)) {
		t.Fatalf("object holds %d bytes, %v", len(data), err)
	}
	os.Remove(shmDir + strings.TrimPrefix(string(name), "/"))
	if !term.Update(uv.KittyGraphicsEvent{Options: kitty.Options{ID: int(s.ids[0])}, Payload: []byte("EBADF:Failed to read image file")}) || term.shared() {
		t.Fatal("a failed read left shared memory on")
	}
	out = s.Encode(ramp(8, 4), 8, 4, 0, 0, 2, 1)
	if !strings.Contains(out, "o=z") {
		t.Fatalf("after the failure pixels still go through shared memory %q", out[:40])
	}
}

func BenchmarkKittyShared(b *testing.B) {
	if !shmWorks() {
		b.Skip("no shared memory objects here")
	}
	term := &Terminal{program: "kitty", placeholders: true, kitty: true, shm: true, nextID: firstID}
	s := term.NewSurface()
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		s.Encode(rgb, w, h, 0, 0, 136, 48)
	}
	b.StopTimer()
	matches, _ := filepath.Glob(shmDir + "icebath-*")
	for _, m := range matches {
		os.Remove(m)
	}
}
