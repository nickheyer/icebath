package tty

import (
	"strings"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// One grid column, a cluster's first column holding its text and width, the rest holding width zero
type Cell struct {
	Text  string
	Width uint8
	Style Style
	Link  uint32
	Rider string
	After string
}

// Column the toolkit leaves untouched, a space in the default style
var blankCell = Cell{Text: " ", Width: 1}

// Column covered by the cluster to its left
var coveredCell = Cell{}

// Single byte strings for ASCII, so the parser never allocates for plain text
var asciiText = func() (t [128]string) {
	for i := range t {
		t[i] = string(rune(i))
	}
	return t
}()

// Reports whether the column continues a wide cluster
func (c *Cell) covered() bool { return c.Width == 0 }

// Cell reduced to what shows, a plain blank keeping only its background and link
func (c Cell) shown() Cell {
	if c.Text == " " && c.Rider == "" && c.After == "" && c.Style.Under == UnderNone && c.Style.Attrs&(AttrReverse|AttrStrike) == 0 {
		c.Style = Style{Bg: c.Style.Bg}
	}
	if c.Width == 0 {
		return coveredCell
	}
	return c
}

// Reports whether two columns hold the same cluster in the same style, link and riders
func sameCell(a, b *Cell) bool {
	if a.Width == 0 || b.Width == 0 {
		return a.Width == b.Width
	}
	return *a == *b
}

// Grid of cells a view asks for, rows of W columns
type Frame struct {
	W, H  int
	Cells []Cell
}

// Row y of the frame
func (f *Frame) Row(y int) []Cell { return f.Cells[y*f.W : (y+1)*f.W] }

// Blank frame of the given size, reusing the cell storage
func (f *Frame) reset(w, h int) {
	f.W, f.H = w, h
	if cap(f.Cells) < w*h {
		f.Cells = make([]Cell, w*h)
	}
	f.Cells = f.Cells[:w*h]
	for i := range f.Cells {
		f.Cells[i] = blankCell
	}
}

// Places a cell at column x of a row, clearing the clusters it overlaps
func putCell(row []Cell, x int, c Cell) {
	w := int(c.Width)
	for i := x; i < x+w && i < len(row); i++ {
		if row[i].covered() {
			for j := i - 1; j >= 0; j-- {
				covered := row[j].covered()
				row[j] = blankCell
				if !covered {
					break
				}
			}
		}
	}
	for i := x + w; i < len(row) && row[i].covered(); i++ {
		row[i] = blankCell
	}
	row[x] = c
	for i := 1; i < w; i++ {
		row[x+i] = coveredCell
	}
}

// Parses view content into the frame with grapheme widths, the method the window manager composes with
type parser struct {
	p       *ansi.Parser
	links   *linkTable
	profile colorprofile.Profile
	colors  map[Color]Color
}

// Parser sharing the renderer's link table
func newParser(links *linkTable) *parser {
	return &parser{p: ansi.NewParser(), links: links, profile: colorprofile.TrueColor, colors: map[Color]Color{}}
}

// Fills the frame from content, lines past the height and clusters past the width left out
func (ps *parser) parse(f *Frame, content string, w, h int) {
	f.reset(w, h)
	if h == 0 || w == 0 || content == "" {
		return
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	var (
		state        byte
		pen          Style
		link         uint32
		x, y         int
		lastX, lastY = -1, -1
		rider        string
	)
	row := f.Row(0)
	str := content
	for len(str) > 0 && y < h {
		seq, width, n, next := ansi.DecodeSequence(str, state, ps.p)
		if n == 1 && seq[0] > 0x1f && seq[0] < 0x7f && len(str) > 1 && str[1] >= 0xc0 {
			if cluster, cw := ansi.FirstGraphemeCluster(str, ansi.GraphemeWidth); len(cluster) > 1 {
				seq, width, n = cluster, cw, len(cluster)
			}
		}
		switch {
		case width > 0:
			if x+width <= w {
				text := seq
				if len(seq) == 1 && seq[0] < 0x80 {
					text = asciiText[seq[0]]
				}
				putCell(row, x, Cell{Text: text, Width: uint8(width), Style: convertStyle(pen, ps.profile, ps.colors), Link: link, Rider: rider})
				lastX, lastY = x, y
			}
			rider = ""
			x += width
		case seq == "\n":
			y++
			x = 0
			if y < h {
				row = f.Row(y)
			}
		case seq == "\r":
			x = 0
		case ansi.HasCsiPrefix(seq) && ps.p.Command() == 'm':
			readSGR(ps.p.Params(), &pen)
		case ansi.HasOscPrefix(seq) && ps.p.Command() == 8:
			link = ps.readLink(ps.p.Data())
		case len(seq) > 0 && seq[0] >= 0xc0:
			if lastY == y && lastX >= 0 {
				row[lastX].Text += seq
			}
		case rideable(seq):
			rider += seq
		}
		state = next
		str = str[n:]
	}
	if rider != "" && lastY >= 0 && lastY < h {
		c := &f.Row(lastY)[lastX]
		c.After += rider
	}
}

// Link id of an OSC 8 body, zero for a closing one
func (ps *parser) readLink(data []byte) uint32 {
	parts := strings.SplitN(string(data), ";", 3)
	if len(parts) != 3 {
		return 0
	}
	return ps.links.id(parts[2], parts[1])
}

// Reports whether a sequence travels with the next cell, a terminated string sequence that changes no cell or lasting state
func rideable(seq string) bool {
	if !ansi.HasApcPrefix(seq) && !ansi.HasDcsPrefix(seq) && !ansi.HasSosPrefix(seq) && !ansi.HasPmPrefix(seq) {
		return false
	}
	n := len(seq)
	if n < 2 || seq[n-1] != '\\' || seq[n-2] != ansi.ESC {
		return false
	}
	e := effectOf([]byte(seq))
	return !e.edits && !e.sticky
}
