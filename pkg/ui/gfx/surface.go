package gfx

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// Base64 characters one kitty chunk carries, the most the protocol allows
const kittyChunk = 4096

// Z index that puts a kitty placement under the text and over the cell background
const kittyUnderText = -1

// One image shown over a box of cells, redrawn frame after frame
type Surface struct {
	t     *Terminal
	proto Protocol
	ids   [2]uint32
	cur   int
	// Id transmitted last and the id the cells show, one frame behind so the image is whole when the cells reach the terminal
	pending, shown uint32
	// Box of cells the last encode covered, the count of frames sent and the pixels of the last one sent
	x, y, cols, rows int
	frame            int
	last             []byte
	zw               *zlib.Writer
	comp, out, pngb  bytes.Buffer
	b64              []byte
	png              png.Encoder
	six              sixel
	img              *image.RGBA
}

// Surface drawing through the protocol of the terminal
func (t *Terminal) NewSurface() *Surface {
	s := &Surface{t: t, ids: t.ids(), png: png.Encoder{CompressionLevel: png.BestSpeed}}
	s.zw, _ = zlib.NewWriterLevel(&s.comp, zlib.BestSpeed)
	return s
}

// Protocol the surface drew with last
func (s *Surface) Protocol() Protocol { return s.proto }

// Escape text that hands a frame of w by h pixels, three bytes each, to the terminal to show over cols by rows cells at a cell, empty when it draws none
func (s *Surface) Encode(rgb []byte, w, h, x, y, cols, rows int) string {
	proto := s.t.Protocol()
	if proto != s.proto {
		s.out.Reset()
		s.out.WriteString(s.release())
		s.proto = proto
	} else {
		s.out.Reset()
	}
	moved := s.x != x || s.y != y || s.cols != cols || s.rows != rows
	s.x, s.y, s.cols, s.rows = x, y, cols, rows
	if proto == None || w <= 0 || h <= 0 || cols <= 0 || rows <= 0 || len(rgb) < w*h*3 {
		return s.t.wrap(s.out.String())
	}
	if !moved && proto == s.proto && s.shown != 0 && bytes.Equal(rgb[:w*h*3], s.last) {
		s.pending = s.shown
		return s.t.wrap(s.out.String())
	}
	s.last = append(s.last[:0], rgb[:w*h*3]...)
	switch proto {
	case KittyCells, KittyPlaced:
		id := s.ids[s.cur]
		s.cur ^= 1
		s.kitty(rgb[:w*h*3], w, h, x, y, cols, rows, id, proto == KittyCells)
		s.pending = id
	case Sixel:
		s.cursorTo(x, y)
		s.out.WriteString("\x1bP0;1;q")
		s.six.encode(&s.out, rgb, w, h)
		s.out.WriteString("\x1b\\")
		s.cursorBack()
		s.pending, s.shown = 1, 1
	case ITerm:
		s.cursorTo(x, y)
		s.iterm(rgb, w, h, cols, rows)
		s.cursorBack()
		s.pending, s.shown = 1, 1
	}
	return s.t.wrap(s.out.String())
}

// Writes the kitty transmission of the pixels, through shared memory on the same machine and else zlib packed in chunks, with a virtual placement for placeholder cells or a placement at the cursor
func (s *Surface) kitty(rgb []byte, w, h, x, y, cols, rows int, id uint32, cells bool) {
	head := []string{"a=T", fmt.Sprintf("i=%d", id), "f=24", fmt.Sprintf("s=%d", w), fmt.Sprintf("v=%d", h), fmt.Sprintf("c=%d", cols), fmt.Sprintf("r=%d", rows)}
	if cells {
		head = append(head, "U=1")
	} else {
		head = append(head, "C=1", fmt.Sprintf("z=%d", kittyUnderText))
		s.cursorTo(x, y)
	}
	if s.t.shared() {
		s.frame++
		name := fmt.Sprintf("icebath-%d-%d-%d", os.Getpid(), id, s.frame)
		if err := shmPut(name, rgb); err == nil {
			head = append(head, "t=s", fmt.Sprintf("S=%d", len(rgb)), "q=1")
			s.out.WriteString("\x1b_G")
			s.out.WriteString(strings.Join(head, ","))
			s.out.WriteString(";")
			s.out.WriteString(base64.StdEncoding.EncodeToString([]byte("/" + name)))
			s.out.WriteString("\x1b\\")
			if !cells {
				s.cursorBack()
			}
			return
		}
	}
	head = append(head, "o=z", "q=2")
	s.comp.Reset()
	s.zw.Reset(&s.comp)
	s.zw.Write(rgb)
	s.zw.Close()
	n := base64.StdEncoding.EncodedLen(s.comp.Len())
	if cap(s.b64) < n {
		s.b64 = make([]byte, n)
	}
	s.b64 = s.b64[:n]
	base64.StdEncoding.Encode(s.b64, s.comp.Bytes())
	for off := 0; off < n; off += kittyChunk {
		end := min(off+kittyChunk, n)
		s.out.WriteString("\x1b_G")
		if off == 0 {
			s.out.WriteString(strings.Join(head, ","))
		} else {
			s.out.WriteString("q=2")
		}
		if end < n {
			s.out.WriteString(",m=1;")
		} else {
			s.out.WriteString(",m=0;")
		}
		s.out.Write(s.b64[off:end])
		s.out.WriteString("\x1b\\")
	}
	if !cells {
		s.cursorBack()
	}
}

// Writes the iTerm2 inline image of the pixels as a PNG sized to the cells
func (s *Surface) iterm(rgb []byte, w, h, cols, rows int) {
	if s.img == nil || s.img.Rect.Dx() != w || s.img.Rect.Dy() != h {
		s.img = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	pix := s.img.Pix
	for i, j := 0, 0; i < w*h*3; i, j = i+3, j+4 {
		pix[j], pix[j+1], pix[j+2], pix[j+3] = rgb[i], rgb[i+1], rgb[i+2], 0xff
	}
	s.pngb.Reset()
	s.png.Encode(&s.pngb, s.img)
	n := base64.StdEncoding.EncodedLen(s.pngb.Len())
	if cap(s.b64) < n {
		s.b64 = make([]byte, n)
	}
	s.b64 = s.b64[:n]
	base64.StdEncoding.Encode(s.b64, s.pngb.Bytes())
	fmt.Fprintf(&s.out, "\x1b]1337;File=inline=1;size=%d;width=%d;height=%d;preserveAspectRatio=0;doNotMoveCursor=1:", s.pngb.Len(), cols, rows)
	s.out.Write(s.b64)
	s.out.WriteString("\x07")
}

// Saves the cursor and moves it to a cell
func (s *Surface) cursorTo(x, y int) {
	s.out.WriteString("\x1b7")
	s.out.WriteString(ansi.CursorPosition(x+1, y+1))
}

// Brings the cursor back to where it was saved
func (s *Surface) cursorBack() { s.out.WriteString("\x1b8") }

// Fills the cells of the box as the image needs them under it, placeholders naming the image for kitty cells and blank cells on the default background otherwise
func (s *Surface) Paint(buf *uv.Buffer, x, y, cols, rows int, bg color.Color) {
	switch s.proto {
	case KittyCells:
		id := s.shown
		s.shown = s.pending
		if id == 0 {
			fillBox(buf, x, y, cols, rows, uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}})
			return
		}
		fg := color.RGBA{R: uint8(id >> 16), G: uint8(id >> 8), B: uint8(id), A: 0xff}
		high := ""
		if id>>24 != 0 {
			high = string(kitty.Diacritic(int(id >> 24)))
		}
		for r := range rows {
			row := string(kitty.Diacritic(r))
			for c := range cols {
				var sb strings.Builder
				sb.WriteRune(kitty.Placeholder)
				sb.WriteString(row)
				if c < diacriticCount() {
					sb.WriteString(string(kitty.Diacritic(c)))
					sb.WriteString(high)
				}
				buf.SetCell(x+c, y+r, &uv.Cell{Content: sb.String(), Width: 1, Style: uv.Style{Fg: fg, Bg: bg}})
			}
		}
	case None:
		fillBox(buf, x, y, cols, rows, uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}})
	default:
		fillBox(buf, x, y, cols, rows, uv.Cell{Content: " ", Width: 1})
	}
}

// Reports whether a cell is a kitty placeholder, which keeps its colors as they name the image and must stay as painted
func Placeholder(c *uv.Cell) bool {
	return c != nil && strings.HasPrefix(c.Content, string(kitty.Placeholder))
}

// Count of distinct row and column diacritics the kitty table holds
func diacriticCount() int {
	n := 1
	for kitty.Diacritic(n) != kitty.Diacritic(0) {
		n++
	}
	return n
}

// Writes one cell over every cell of a box
func fillBox(buf *uv.Buffer, x, y, cols, rows int, c uv.Cell) {
	for r := range rows {
		for k := range cols {
			buf.SetCell(x+k, y+r, &c)
		}
	}
}

// Escape text that takes the images of the surface off the terminal
func (s *Surface) Release() string { return s.t.wrap(s.release()) }

// Removes every shared memory object this process left behind, for the end of the program
func Sweep() { shmSweep() }

func (s *Surface) release() string {
	shmSweep()
	switch s.proto {
	case KittyCells, KittyPlaced:
		s.pending, s.shown, s.last = 0, 0, s.last[:0]
		return ansi.KittyGraphics(nil, "a=d", "d=I", fmt.Sprintf("i=%d", s.ids[0]), "q=2") + ansi.KittyGraphics(nil, "a=d", "d=I", fmt.Sprintf("i=%d", s.ids[1]), "q=2")
	case Sixel, ITerm:
		s.pending, s.shown, s.last = 0, 0, s.last[:0]
		if s.cols <= 0 || s.rows <= 0 {
			return ""
		}
		var sb strings.Builder
		sb.WriteString("\x1b7")
		for r := range s.rows {
			sb.WriteString(ansi.CursorPosition(s.x+1, s.y+r+1))
			sb.WriteString(strings.Repeat(" ", s.cols))
		}
		sb.WriteString("\x1b8")
		return sb.String()
	}
	return ""
}
