package tty

import (
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

func TestParseMatchesTheToolkitGrid(t *testing.T) {
	const w, h = 60, 8
	buf := uv.NewBuffer(w, h)
	scr := refScreen{buf}
	link := uv.NewLink("https://example.com/x", "id=7")
	put := func(x, y int, s string, st uv.Style) {
		for _, r := range s {
			c := uv.NewCell(scr.WidthMethod(), string(r))
			c.Style = st
			buf.SetCell(x, y, c)
			x += c.Width
		}
	}
	put(0, 0, "plain text", uv.Style{})
	put(0, 1, "bold red", uv.Style{Fg: lipgloss.Color("#ff0000"), Attrs: uv.AttrBold})
	put(0, 2, "中文 wide", uv.Style{Bg: lipgloss.Color("#102030")})
	put(0, 3, "curly", uv.Style{Underline: uv.UnderlineCurly, UnderlineColor: lipgloss.Color("#00ff00")})
	c := uv.NewCell(scr.WidthMethod(), "L")
	c.Link = link
	buf.SetCell(0, 4, c)
	put(0, 5, "é ñ", uv.Style{Attrs: uv.AttrItalic | uv.AttrStrikethrough})
	content := buf.Render()
	links := newLinkTable()
	ps := newParser(links)
	var f Frame
	ps.parse(&f, content, w, h)
	ref := reference(content, w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			got, want := f.Row(y)[x], fromUV(ref.CellAt(x, y), links)
			if !sameCell(&got, &want) {
				t.Fatalf("(%d,%d) parsed %+v, toolkit has %+v", x, y, got, want)
			}
		}
	}
}

func TestStringSequencesRideOnCells(t *testing.T) {
	links := newLinkTable()
	ps := newParser(links)
	var f Frame
	ps.parse(&f, "a\x1b_Gi=1\x1b\\b\x1b_Gz\x1b\\", 10, 1)
	if r := f.Row(0)[1]; r.Text != "b" || r.Rider != "\x1b_Gi=1\x1b\\" {
		t.Fatalf("rider landed as %+v", r)
	}
	if r := f.Row(0)[1]; r.After != "\x1b_Gz\x1b\\" {
		t.Fatalf("trailing sequence landed as %+v", r)
	}
}

func TestClusterPastTheEdgeIsLeftOut(t *testing.T) {
	ps := newParser(newLinkTable())
	var f Frame
	ps.parse(&f, "abc中", 4, 1)
	if c := f.Row(0)[3]; c.Text != " " || c.Width != 1 {
		t.Fatalf("wide cluster at the last column parsed as %+v", c)
	}
}
