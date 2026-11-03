package cards

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Writes text, boxes and panels into the cells of a painted card in the theme
type Painter struct {
	buf cells
	t   theme.Theme
	w   int
	h   int
}

func NewPainter(buf *uv.Buffer, t theme.Theme) *Painter {
	return &Painter{buf: cells{buf}, t: t, w: buf.Width(), h: buf.Height()}
}

// Cells the painter writes into
func (p *Painter) Cells() *uv.Buffer { return p.buf.Buffer }

// Theme the painter draws in
func (p *Painter) Theme() theme.Theme { return p.t }

// Width and height of the cells
func (p *Painter) Size() (w, h int) { return p.w, p.h }

// Tints a box toward a color by a share, text under it kept, for a panel laid over a drawing
func (p *Painter) Tint(x, y, w, h int, bg color.Color, share float64) {
	for cy := max(y, 0); cy < min(y+h, p.h); cy++ {
		for cx := max(x, 0); cx < min(x+w, p.w); cx++ {
			c := p.buf.CellAt(cx, cy)
			if c == nil {
				continue
			}
			out := *c
			if p.t.Plain {
				out = uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}}
			} else {
				out.Style.Bg = theme.Mix(orColor(c.Style.Bg, p.t.Bg), bg, share)
				if c.Style.Fg != nil {
					out.Style.Fg = theme.Mix(c.Style.Fg, bg, share)
				}
			}
			p.buf.SetCell(cx, cy, &out)
		}
	}
}

// Blends the background of a box toward a color, the text and its colors kept
func (p *Painter) TintBg(x, y, w, h int, bg color.Color) {
	for cy := max(y, 0); cy < min(y+h, p.h); cy++ {
		for cx := max(x, 0); cx < min(x+w, p.w); cx++ {
			c := p.buf.CellAt(cx, cy)
			if c == nil {
				continue
			}
			out := *c
			out.Style.Bg = bg
			p.buf.SetCell(cx, cy, &out)
		}
	}
}

// Fills a box with one color, dropping what was there
func (p *Painter) Fill(x, y, w, h int, bg color.Color) {
	c := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}}
	for cy := max(y, 0); cy < min(y+h, p.h); cy++ {
		for cx := max(x, 0); cx < min(x+w, p.w); cx++ {
			p.buf.SetCell(cx, cy, &c)
		}
	}
}

// Writes plain text at a cell in a color over whatever background is there, cut at the right edge
func (p *Painter) Text(x, y int, s string, fg color.Color, bold bool) int {
	if y < 0 || y >= p.h {
		return 0
	}
	n := 0
	for _, g := range Graphemes(s) {
		if x+n >= p.w {
			break
		}
		if x+n < 0 {
			n++
			continue
		}
		bg := color.Color(p.t.Bg)
		if have := p.buf.CellAt(x+n, y); have != nil && have.Style.Bg != nil {
			bg = have.Style.Bg
		}
		c := uv.Cell{Content: g, Width: 1, Style: uv.Style{Fg: fg, Bg: bg}}
		if bold {
			c.Style.Attrs = uv.AttrBold
		}
		p.buf.SetCell(x+n, y, &c)
		n++
	}
	return n
}

// Writes text with its own background, cut at the right edge
func (p *Painter) Label(x, y int, s string, fg, bg color.Color, bold bool) int {
	if y < 0 || y >= p.h {
		return 0
	}
	n := 0
	for _, g := range Graphemes(s) {
		if x+n >= p.w {
			break
		}
		if x+n >= 0 {
			c := uv.Cell{Content: g, Width: 1, Style: uv.Style{Fg: fg, Bg: bg}}
			if bold {
				c.Style.Attrs = uv.AttrBold
			}
			p.buf.SetCell(x+n, y, &c)
		}
		n++
	}
	return n
}

// Draws a styled string, as lipgloss renders it, into a box
func (p *Painter) Styled(x, y, w, h int, s string) {
	if w <= 0 || h <= 0 {
		return
	}
	uv.NewStyledString(s).Draw(p.buf, uv.Rect(x, y, w, h))
}

// Draws a bordered panel with a title over the cells, tinted by a share, its body left for the caller
func (p *Painter) Panel(x, y, w, h int, title string, share float64) {
	t := p.t
	p.Tint(x, y, w, h, t.Surface, share)
	b := t.Border
	edge := uv.Style{Fg: t.SteelBright}
	put := func(cx, cy int, s string) {
		if cx < 0 || cy < 0 || cx >= p.w || cy >= p.h {
			return
		}
		c := p.buf.CellAt(cx, cy)
		cell := uv.Cell{Content: s, Width: 1, Style: edge}
		if c != nil {
			cell.Style.Bg = c.Style.Bg
		}
		p.buf.SetCell(cx, cy, &cell)
	}
	for i := 1; i < w-1; i++ {
		put(x+i, y, b.Top)
		put(x+i, y+h-1, b.Bottom)
	}
	for j := 1; j < h-1; j++ {
		put(x, y+j, b.Left)
		put(x+w-1, y+j, b.Right)
	}
	put(x, y, b.TopLeft)
	put(x+w-1, y, b.TopRight)
	put(x, y+h-1, b.BottomLeft)
	put(x+w-1, y+h-1, b.BottomRight)
	if title != "" && w > 6 {
		p.Text(x+2, y, " "+ansi.Truncate(title, w-6, "")+" ", t.Ice, true)
	}
}

// Writes lines down a box, each cut to its width, the rest of the box left as it is
func (p *Painter) Lines(x, y, w, h int, lines []string) {
	for i, line := range lines {
		if i >= h {
			break
		}
		p.Styled(x, y+i, w, 1, ansi.Truncate(line, w, p.t.Glyphs.Ellipsis))
	}
}

// Column of grip marks along an edge that drags, lit while the pointer holds it
func (p *Painter) Handle(x, y, h int, hot bool) {
	t := p.t
	fg, glyph := t.Steel, t.Glyphs.Sep
	if hot {
		fg, glyph = t.Ice, t.Glyphs.Heavy
	}
	for cy := y; cy < y+h; cy++ {
		p.Label(x, cy, glyph, fg, t.Surface, false)
	}
}

// Grapheme clusters of a string, each one cell wide or wider
func Graphemes(s string) []string {
	var out []string
	for s != "" {
		g, _ := ansi.FirstGraphemeCluster(s, ansi.GraphemeWidth)
		if g == "" {
			g = s
		}
		out = append(out, g)
		s = s[len(g):]
	}
	return out
}

// Lines of styled text wrapped to a width, every line after the first led by an indent of spaces
func WrapIndent(text string, w, indent int) []string {
	if w <= 0 {
		return []string{text}
	}
	first := strings.Split(render.Wrap(text, w), "\n")
	if indent <= 0 || len(first) == 1 {
		return first
	}
	out := []string{first[0]}
	rest := strings.Join(first[1:], " ")
	for _, l := range strings.Split(render.Wrap(rest, max(w-indent, 1)), "\n") {
		out = append(out, strings.Repeat(" ", indent)+l)
	}
	return out
}

// Text wrapped to a width in a color
func WrapStyled(text string, w int, fg color.Color) []string {
	style := lipgloss.NewStyle().Foreground(fg)
	var out []string
	for _, l := range WrapIndent(text, w, 0) {
		out = append(out, style.Render(l))
	}
	return out
}

// Color, or the fallback while it is nil
func orColor(c, fallback color.Color) color.Color {
	if c == nil {
		return fallback
	}
	return c
}
