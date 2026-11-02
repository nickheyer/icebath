package sprint

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/theme"
)

// Cell buffer that measures text by grapheme width so it matches lipgloss
type screen struct{ *uv.Buffer }

func (screen) WidthMethod() uv.WidthMethod { return uv.WidthMethod(ansi.GraphemeWidth) }

// Writes text and panels over the cells of a frame, blending panels into what the frame already shows
type painter struct {
	buf screen
	t   theme.Theme
	w   int
	h   int
}

func newPainter(buf *uv.Buffer, t theme.Theme) *painter {
	return &painter{buf: screen{buf}, t: t, w: buf.Width(), h: buf.Height()}
}

// Tints a box toward a color by a share, text under it kept, for a panel laid over the floor
func (p *painter) tint(x, y, w, h int, bg color.Color, share float64) {
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
				out.Style.Bg = theme.Mix(or(c.Style.Bg, p.t.Bg), bg, share)
				if c.Style.Fg != nil {
					out.Style.Fg = theme.Mix(c.Style.Fg, bg, share)
				}
			}
			p.buf.SetCell(cx, cy, &out)
		}
	}
}

// Fills a box with one color, dropping what was there
func (p *painter) fill(x, y, w, h int, bg color.Color) {
	c := uv.Cell{Content: " ", Width: 1, Style: uv.Style{Bg: bg}}
	for cy := max(y, 0); cy < min(y+h, p.h); cy++ {
		for cx := max(x, 0); cx < min(x+w, p.w); cx++ {
			p.buf.SetCell(cx, cy, &c)
		}
	}
}

// Writes plain text at a cell in a color over whatever background is there, cut at the right edge
func (p *painter) text(x, y int, s string, fg color.Color, bold bool) int {
	if y < 0 || y >= p.h {
		return 0
	}
	n := 0
	for _, g := range graphemes(s) {
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
func (p *painter) label(x, y int, s string, fg, bg color.Color, bold bool) int {
	if y < 0 || y >= p.h {
		return 0
	}
	n := 0
	for _, g := range graphemes(s) {
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
func (p *painter) styled(x, y, w, h int, s string) {
	if w <= 0 || h <= 0 {
		return
	}
	uv.NewStyledString(s).Draw(p.buf, uv.Rect(x, y, w, h))
}

// Draws a bordered panel with a title over the frame, its body left for the caller
func (p *painter) panel(x, y, w, h int, title string, share float64) {
	t := p.t
	p.tint(x, y, w, h, t.Surface, share)
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
		p.text(x+2, y, " "+ansi.Truncate(title, w-6, "")+" ", t.Ice, true)
	}
}

// Writes lines down a box, each cut to its width, the rest of the box left as it is
func (p *painter) lines(x, y, w, h int, lines []string) {
	for i, line := range lines {
		if i >= h {
			break
		}
		p.styled(x, y+i, w, 1, ansi.Truncate(line, w, p.t.Glyphs.Ellipsis))
	}
}

// Bar of a share in a width with the theme fill and track marks
func bar(share float64, width int, t theme.Theme) string {
	if width <= 0 {
		return ""
	}
	n := int(min(max(share, 0), 1)*float64(width) + 0.5)
	return lipgloss.NewStyle().Foreground(t.Alive).Render(strings.Repeat(t.Glyphs.Fill, n)) + lipgloss.NewStyle().Foreground(t.Steel).Render(strings.Repeat(t.Glyphs.Empty, width-n))
}

// Grapheme clusters of a string, each one cell wide or wider
func graphemes(s string) []string {
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

func or(c, fallback color.Color) color.Color {
	if c == nil {
		return fallback
	}
	return c
}
